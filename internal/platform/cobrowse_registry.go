// internal/platform/cobrowse_registry.go
//
// Durable co-browse session registry and re-adoption (citadel-cli#1546).
//
// The live session map is process memory, and a self-update restart is a
// syscall.Exec: same PID, new image, empty memory. The browsers survive that
// (they are separate processes), but nothing knew they existed, so the next
// sweep killed them as orphans. This file persists one small record per
// session inside its own owner-only session dir, and recoverSessions re-adopts
// every record that still verifies, under the same session id, BEFORE the
// orphan sweep runs.
//
// Records live in a registry dir separate from the session (profile) dirs, by
// default under the node's durable config dir (see SetRegistryDirResolver), so
// they outlive a temp-dir cleanup and carry a lifecycle state (live, suspended,
// expired) and a generation counter for later suspend/resume work. Only live
// records are ever adopted; relaunching a dead browser is out of scope here.
//
// Adoption is Linux only (it needs /proc start times). Windows keeps its
// kill-on-close Job Object behavior: sessions die with the process, and the
// restart marker (cobrowse_ended.go) says why.
package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const sessionRecordVersion = 1

// Registry record states. suspended is reserved for the suspend/resume feature:
// nothing produces it yet, but the sweep must never delete such a record.
const (
	RecordLive      = "live"
	RecordSuspended = "suspended"
	RecordExpired   = "expired"
)

const (
	registryDirSuffix     = ".registry"
	expiredRetention      = 24 * time.Hour
	expiredMaxRecords     = 64
	endedReasonLost       = "process_gone"
	EndedReasonNodeUpdate = "node_update"
)

// registryDirResolver names the durable registry dir. cmd points it at a subdir
// of the node config dir (platform cannot import network). It is the ONE place
// the registry location is chosen.
var registryDirResolver func() string

// SetRegistryDirResolver sets where session registry records live.
func SetRegistryDirResolver(f func() string) { registryDirResolver = f }

// registryDir resolves this manager's registry dir. Without a resolver (tests,
// local mode) it is a sibling of the session base dir, never inside it: the
// orphan sweep treats every subdirectory of the base dir as a session.
func (m *CobrowseSessionManager) registryDir() string {
	if m.useResolver && registryDirResolver != nil {
		if d := registryDirResolver(); d != "" {
			return d
		}
	}
	return filepath.Clean(m.baseDir) + registryDirSuffix
}

// sessionRegDir is the registry dir for new sessions: only the node-wide
// manager keeps records. Local mode and other isolated managers never do, so
// nothing can adopt their browsers (their proxy guard dies with their process).
func (m *CobrowseSessionManager) sessionRegDir() string {
	if !m.useResolver {
		return ""
	}
	return m.registryDir()
}

var errCDPCheck = errors.New("debug endpoint check failed")

var sessionIDPattern = regexp.MustCompile(`^cb-[0-9a-f]+$`)

// sessionRecord is what a later process needs to re-adopt a session. It holds
// no credentials: the profile is referenced by path only.
type sessionRecord struct {
	Version      int    `json:"version"`
	ID           string `json:"id"`
	Profile      string `json:"profile"`
	Persistent   bool   `json:"persistent,omitempty"`
	Browser      string `json:"browser,omitempty"`
	Display      string `json:"display,omitempty"`
	DebugPort    int    `json:"debug_port"`
	BrowserPID   int    `json:"browser_pid"`
	BrowserStart uint64 `json:"browser_start"`
	XvfbPID      int    `json:"xvfb_pid,omitempty"`
	XvfbStart    uint64 `json:"xvfb_start,omitempty"`
	OwnerPID     int    `json:"owner_pid"`
	OwnerStart   uint64 `json:"owner_start"`
	StartedAt    string `json:"started_at,omitempty"`
	// BrowserScope and XvfbScope name the transient systemd scopes the processes
	// run in (empty when launched directly, which makes the record un-adoptable
	// once its owner is gone). ScopeUser says the user manager owns them. OwnerUID
	// is the OS user that wrote the record.
	BrowserScope string `json:"browser_scope,omitempty"`
	XvfbScope    string `json:"xvfb_scope,omitempty"`
	ScopeUser    bool   `json:"scope_user,omitempty"`
	OwnerUID     *int   `json:"owner_uid,omitempty"`
	Handoff      bool   `json:"handoff,omitempty"`

	// State is live, suspended or expired. Generation increases on every
	// re-adopt or ownership change so a later owner can order its claims.
	State      string `json:"state"`
	Generation uint64 `json:"generation"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	// PendingEnd is set just before a self-restart: if the session does not
	// survive, this becomes its ended_reason.
	PendingEnd  *EndedSession `json:"pending_end,omitempty"`
	EndedReason string        `json:"ended_reason,omitempty"`
	EndedDetail string        `json:"ended_detail,omitempty"`
	EndedAt     string        `json:"ended_at,omitempty"`
}

// adoptionSupported reports whether this OS can re-adopt sessions. A var so
// tests exercise the logic on any host.
var adoptionSupported = runtime.GOOS == "linux"

// procIdentity returns the kernel start time of pid and whether it is a live,
// non-zombie process. A start time pins identity: a recycled PID has a
// different one. Package var so tests inject a fake process table.
var procIdentity = defaultProcIdentity

func defaultProcIdentity(pid int) (start uint64, alive bool, err error) {
	if pid <= 0 {
		return 0, false, fmt.Errorf("bad pid %d", pid)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false, err
	}
	return parseProcStat(string(data))
}

// parseProcStat extracts state and starttime from /proc/<pid>/stat. The comm
// field may contain spaces and parens, so parse from the LAST ')'.
func parseProcStat(s string) (uint64, bool, error) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false, fmt.Errorf("malformed stat")
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3 (state); starttime is field 22 -> f[19].
	if len(f) < 20 {
		return 0, false, fmt.Errorf("short stat")
	}
	st, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return 0, false, err
	}
	return st, f[0] != "Z" && f[0] != "X", nil
}

// currentOwnerIdentity is this process's own start time (0 when unavailable).
func currentOwnerIdentity() uint64 {
	st, _, err := procIdentity(os.Getpid())
	if err != nil {
		return 0
	}
	return st
}

// adoptCDPCheck confirms the browser behind a recorded port is the one whose
// id its own profile dir names. Package var for tests.
var adoptCDPCheck = defaultAdoptCDPCheck

func defaultAdoptCDPCheck(port int, profileDir string) error {
	data, err := os.ReadFile(filepath.Join(profileDir, devToolsActivePortFile))
	if err != nil {
		return err
	}
	p, id, err := parseDevToolsActivePort(data)
	if err != nil {
		return err
	}
	if p != port {
		return fmt.Errorf("recorded debug port %d does not match the profile's %d", port, p)
	}
	return verifyCDPBrowserID(port, id)
}

func recordFile(regDir, id string) string { return filepath.Join(regDir, id+".json") }

// writeFileAtomic writes data owner-only through a temp file and rename.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil { // best effort: persist the rename
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// saveRecord writes rec into regDir, creating it owner-only (with the Windows
// DACL helper) if needed. The pure core: tests pass a temp dir.
func saveRecord(regDir string, rec sessionRecord) error {
	if !sessionIDPattern.MatchString(rec.ID) {
		return fmt.Errorf("bad session id")
	}
	if err := ensureCobrowseBaseDir(regDir); err != nil {
		return err
	}
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writeFileAtomic(recordFile(regDir, rec.ID), data)
}

func loadRecord(regDir, id string) (sessionRecord, error) {
	var rec sessionRecord
	data, err := os.ReadFile(recordFile(regDir, id))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, err
	}
	if rec.ID != id {
		return rec, fmt.Errorf("record id does not match its file")
	}
	return rec, nil
}

// listRecords returns every parseable record. It never creates the dir, and
// refuses one that is not owner-trusted.
func listRecords(regDir string) []sessionRecord {
	if _, err := os.Lstat(regDir); err != nil {
		return nil
	}
	if ensureCobrowseBaseDir(regDir) != nil {
		return nil
	}
	entries, err := os.ReadDir(regDir)
	if err != nil {
		return nil
	}
	var out []sessionRecord
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 10*time.Minute {
				_ = os.Remove(filepath.Join(regDir, e.Name())) // crash leftover
			}
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if e.IsDir() || id == e.Name() || !sessionIDPattern.MatchString(id) {
			continue
		}
		if rec, err := loadRecord(regDir, id); err == nil {
			out = append(out, rec)
		}
	}
	return out
}

func deleteRecord(regDir, id string) { _ = os.Remove(recordFile(regDir, id)) }

// persistRecord writes (best-effort) the registry record for a live session.
func (s *cobrowseSession) persistRecord() {
	s.mu.Lock()
	proc := s.proc
	if proc == nil || !adoptionSupported || s.regDir == "" {
		s.mu.Unlock()
		return
	}
	rec := sessionRecord{
		Version:      sessionRecordVersion,
		ID:           s.id,
		Profile:      s.profile,
		Persistent:   s.persistent,
		Browser:      proc.browser,
		Display:      proc.display,
		DebugPort:    proc.debugPort,
		BrowserPID:   proc.browserPID,
		XvfbPID:      proc.xvfbPID,
		OwnerPID:     os.Getpid(),
		BrowserScope: proc.browserScope,
		XvfbScope:    proc.xvfbScope,
		ScopeUser:    proc.scopeUser,
		Handoff:      s.explicitHandoff,
		State:        RecordLive,
		Generation:   s.generation,
		PendingEnd:   s.pendingEnd,
	}
	regDir := s.regDir
	if !s.startedAt.IsZero() {
		rec.StartedAt = s.startedAt.UTC().Format(time.RFC3339)
	}
	s.mu.Unlock()
	uid := os.Getuid()
	rec.OwnerUID = &uid
	rec.OwnerStart = currentOwnerIdentity()
	rec.BrowserStart, _, _ = procIdentity(rec.BrowserPID)
	if rec.XvfbPID > 0 {
		rec.XvfbStart, _, _ = procIdentity(rec.XvfbPID)
	}
	if err := saveRecord(regDir, rec); err != nil {
		log.Printf("[cobrowse] could not persist session record for %s: %v", rec.ID, err)
	}
}

// verifyAdoptable decides whether the record in dir describes a session this
// process may take over. Every check fails closed.
func verifyAdoptable(baseDir, dir string, rec sessionRecord) error {
	if rec.Version != sessionRecordVersion {
		return fmt.Errorf("unknown record version %d", rec.Version)
	}
	if rec.ID == "" || rec.ID != filepath.Base(dir) || rec.Profile != dir ||
		filepath.Dir(dir) != filepath.Clean(baseDir) {
		return fmt.Errorf("record does not match its directory")
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("session dir is not a plain directory")
	}
	// The exec case: the record's owner is THIS process (same PID and start
	// time, both unchanged by exec). Otherwise only the managed worker may take
	// over a record whose owner is gone, and only a detached one.
	if rec.OwnerPID != os.Getpid() || rec.OwnerStart == 0 || rec.OwnerStart != currentOwnerIdentity() {
		if err := verifyDetachedOwner(rec); err != nil {
			return err
		}
	}
	if rec.Persistent {
		return fmt.Errorf("persistent profile sessions are sealed, not adopted")
	}
	if rec.BrowserStart == 0 {
		return fmt.Errorf("browser start time not recorded")
	}
	st, alive, err := procIdentity(rec.BrowserPID)
	if err != nil || !alive || st != rec.BrowserStart {
		return fmt.Errorf("browser pid %d is gone or was reused", rec.BrowserPID)
	}
	if !processMatchesRole(rec.BrowserPID, roleCobrowseBrowser) {
		return fmt.Errorf("browser pid %d no longer looks like a browser", rec.BrowserPID)
	}
	if rec.XvfbPID > 0 {
		st, alive, err := procIdentity(rec.XvfbPID)
		if err != nil || !alive || rec.XvfbStart == 0 || st != rec.XvfbStart ||
			!processMatchesRole(rec.XvfbPID, roleCobrowseXvfb) {
			return fmt.Errorf("Xvfb pid %d is gone or was reused", rec.XvfbPID)
		}
	}
	if rec.DebugPort <= 0 || rec.DebugPort > 65535 {
		return fmt.Errorf("bad debug port")
	}
	if err := adoptCDPCheck(rec.DebugPort, dir); err != nil {
		return fmt.Errorf("%w: %v", errCDPCheck, err)
	}
	return nil
}

// verifyDetachedOwner is the narrow extra rule for a record whose owner died
// (a full service restart). Every check fails closed: the new process is the
// managed worker, the record is from the same OS user, its owner is really gone,
// and its browser and Xvfb are alive in the recorded scopes. The caller then
// still checks start identities, role and the CDP identity as for any record.
func verifyDetachedOwner(rec sessionRecord) error {
	if !detachedWorker.Load() {
		return fmt.Errorf("record was not written by this process and this is not the managed worker")
	}
	if rec.BrowserScope == "" {
		return fmt.Errorf("session was not launched in a detachable scope")
	}
	if rec.OwnerUID == nil || *rec.OwnerUID != os.Getuid() {
		return fmt.Errorf("record was written by a different OS user")
	}
	if st, alive, err := procIdentity(rec.OwnerPID); err == nil && alive && st == rec.OwnerStart {
		return fmt.Errorf("record owner is still running")
	}
	if !inScope(rec.BrowserPID, rec.BrowserScope) {
		return fmt.Errorf("browser pid %d is not in its recorded scope", rec.BrowserPID)
	}
	if rec.XvfbPID > 0 && !inScope(rec.XvfbPID, rec.XvfbScope) {
		return fmt.Errorf("Xvfb pid %d is not in its recorded scope", rec.XvfbPID)
	}
	return nil
}

// stopRecordScopes stops the scopes a record names, identity-checked.
func stopRecordScopes(rec sessionRecord) {
	if rec.XvfbScope != "" {
		_ = stopScope(rec.ScopeUser, rec.BrowserScope, rec.BrowserPID, rec.BrowserStart)
		_ = stopScope(rec.ScopeUser, rec.XvfbScope, rec.XvfbPID, rec.XvfbStart)
		return
	}
	_ = stopScope(rec.ScopeUser, rec.BrowserScope, rec.BrowserPID, rec.BrowserStart)
}

// adoptedProc builds a cobrowseProc around already-running processes. Its
// exited channel closes when the browser's identity stops matching.
func adoptedProc(rec sessionRecord) *cobrowseProc {
	exited := make(chan struct{})
	gone := func(pid int, start uint64) bool {
		st, alive, err := procIdentity(pid)
		return err != nil || !alive || st != start
	}
	go func() {
		for {
			time.Sleep(2 * time.Second)
			if gone(rec.BrowserPID, rec.BrowserStart) {
				reapChild(rec.BrowserPID)
				close(exited)
				return
			}
		}
	}()
	return &cobrowseProc{
		debugPort:  rec.DebugPort,
		display:    rec.Display,
		browser:    rec.Browser,
		browserPID: rec.BrowserPID,
		xvfbPID:    rec.XvfbPID,
		exited:     exited,

		browserScope: rec.BrowserScope,
		xvfbScope:    rec.XvfbScope,
		scopeUser:    rec.ScopeUser,
		stop: func() error {
			stopRecordScopes(rec)
			var last error
			for _, t := range []struct {
				pid   int
				start uint64
			}{{rec.BrowserPID, rec.BrowserStart}, {rec.XvfbPID, rec.XvfbStart}} {
				if t.pid <= 0 || gone(t.pid, t.start) {
					continue
				}
				// A surviving child of ours stays a zombie until reaped, and
				// the killer waits for the PID to vanish; reap alongside it.
				done := make(chan struct{})
				go func(pid int) {
					for i := 0; i < 60; i++ {
						select {
						case <-done:
							return
						case <-time.After(50 * time.Millisecond):
							reapChild(pid)
						}
					}
				}(t.pid)
				if err := pidKiller(t.pid); err != nil {
					last = err
				}
				close(done)
				reapChild(t.pid)
			}
			return last
		},
	}
}

// recoverSessions walks the registry before the orphan sweep. A live record
// that verifies is re-adopted under the same id with a bumped generation; a live
// record that does not is marked expired (never silently deleted); suspended
// records are left alone; expired ones are kept for expiredRetention.
func (m *CobrowseSessionManager) recoverSessions() {
	// Only the node-wide manager recovers. Local mode and isolated managers
	// fall through to the plain orphan sweep.
	if !m.useResolver {
		return
	}
	if err := ensureCobrowseBaseDir(m.baseDir); err != nil {
		return
	}
	regDir := m.registryDir()
	now := time.Now()
	var expired []sessionRecord
	for _, rec := range listRecords(regDir) {
		switch rec.State {
		case RecordSuspended:
			continue
		case RecordExpired:
			if t, err := time.Parse(time.RFC3339, rec.EndedAt); err != nil || now.Sub(t) >= expiredRetention {
				deleteRecord(regDir, rec.ID)
			} else {
				expired = append(expired, rec)
			}
			continue
		case RecordLive:
		default:
			continue
		}
		if !adoptionSupported {
			continue
		}
		// A live record owned by ANOTHER running process is theirs: leave it
		// exactly as it is (a monitor or second invocation must not touch it).
		if rec.OwnerPID != os.Getpid() && rec.OwnerStart != 0 {
			if st, alive, err := procIdentity(rec.OwnerPID); err == nil && alive && st == rec.OwnerStart {
				continue
			}
		}
		dir := filepath.Join(m.baseDir, rec.ID)
		err := verifyAdoptable(m.baseDir, dir, rec)
		if errors.Is(err, errCDPCheck) { // a slow endpoint must not expire a healthy session
			time.Sleep(500 * time.Millisecond)
			err = verifyAdoptable(m.baseDir, dir, rec)
		}
		if err != nil {
			log.Printf("[cobrowse] not adopting session %s: %v", rec.ID, err)
			stopRecordScopes(rec) // identity-checked; the sweep reaps anything left
			rec.State = RecordExpired
			rec.EndedReason, rec.EndedDetail = endedReasonLost, ""
			if rec.PendingEnd != nil {
				rec.EndedReason, rec.EndedDetail = rec.PendingEnd.Reason, rec.PendingEnd.Detail
			}
			rec.PendingEnd = nil
			rec.EndedAt = now.UTC().Format(time.RFC3339)
			_ = saveRecord(regDir, rec)
			expired = append(expired, rec)
			continue
		}
		s := &cobrowseSession{
			id:              rec.ID,
			profile:         dir,
			regDir:          regDir,
			state:           SessionRunning,
			proc:            adoptedProc(rec),
			explicitHandoff: rec.Handoff,
			generation:      rec.Generation + 1,
		}
		if t, err := time.Parse(time.RFC3339, rec.StartedAt); err == nil {
			s.startedAt = t
		}
		sweepProtected.Store(dir, struct{}{})
		m.mu.Lock()
		if _, exists := m.sessions[rec.ID]; !exists {
			m.sessions[rec.ID] = s
		}
		m.mu.Unlock()
		writeSessionPidfile(dir, os.Getpid(), rec.BrowserPID, rec.XvfbPID, currentProcessStartToken())
		s.persistRecord()
		log.Printf("[cobrowse] re-adopted session %s (generation %d)", rec.ID, s.generation)
	}
	if len(expired) > expiredMaxRecords {
		sort.Slice(expired, func(i, j int) bool { return expired[i].EndedAt > expired[j].EndedAt })
		for _, r := range expired[expiredMaxRecords:] {
			deleteRecord(regDir, r.ID)
		}
	}
}

// sweepProtected holds the dirs of sessions this process just re-adopted, so
// the orphan sweep (which treats a dir owned by our own PID as stale) skips them.
var sweepProtected sync.Map

// Recover re-adopts surviving sessions and sweeps the rest. Idempotent; the
// worker calls it at startup, and every session read path calls it lazily.
func (m *CobrowseSessionManager) Recover() { m.sweepOnce.Do(m.sweep) }
