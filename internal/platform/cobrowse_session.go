// internal/platform/cobrowse_session.go
//
// Multi-session interactive browser manager (issue #793). Where GetCobrowseManager()
// owns a SINGLE long-lived browser per node, this manager owns a MAP of concurrent,
// mutually isolated browser sessions -- each its own Chromium process, dedicated
// Xvfb virtual display, CDP debug port, and throwaway per-session profile directory.
//
// This is a deliberate SIBLING of CobrowseManager and MeetingBrowser, not a reuse of
// either: the singleton is single-session by design, and MeetingBrowser hard-requires
// an audio sink and deliberately preserves its profile across runs. This manager
// needs neither -- it launches a plain isolated session with a throwaway profile that
// is removed on stop. It reuses only the package-level, side-effect-free launch
// helpers (buildChromeArgs, startManagedXvfb, findFreeDebugPort, withDisplay,
// findChromium, waitForCDPReady, pickTarget) so there is no duplicated browser-launch
// logic.
//
// Live viewing and input forwarding (screencast + input bridge) are a SEPARATE issue
// (#794); this file builds only the session lifecycle. Status exposes debug_port and
// display so a viewer can attach later, and MarkAttached/MarkDetached flip the session
// state without any streaming transport here.
//
// A session's working profile dir is a private temp dir either way. Without a
// SessionProfile it is throwaway (removed on stop, fresh each time). With one
// (issue #795), the same temp dir is the DECRYPTED working copy of an encrypted,
// PIN-unlocked persistent profile: StartSessionWithProfile materializes the
// profile into it before launch and re-encrypts it on stop, so logins persist
// while only ciphertext lives at rest. The plaintext working copy is still just a
// temp dir, so the existing crash-sweep and teardown removal keep the guarantee
// that no plaintext profile is left behind.
package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cobrowseSessionsDirName is the parent directory (under os.TempDir()) that holds
// every session's throwaway profile dir and pidfile. A single parent lets a fresh
// process sweep orphans left by a SIGKILLed / crashed predecessor (see sweep).
const cobrowseSessionsDirName = "citadel-cobrowse-sessions"

// EnvCobrowseMaxSessions overrides the cap on concurrent browser sessions. Each
// session is a full Chromium + Xvfb (hundreds of MB), so an unbounded StartSession
// is a resource-exhaustion risk; the cap fails a surplus StartSession loudly instead.
const EnvCobrowseMaxSessions = "CITADEL_COBROWSE_MAX_SESSIONS"

// defaultMaxCobrowseSessions is the concurrent-session cap when EnvCobrowseMaxSessions
// is unset.
const defaultMaxCobrowseSessions = 8

// sessionPidfileName is the per-session file recording the browser and Xvfb PIDs so a
// later process can reap them after a crash that skipped graceful teardown.
const sessionPidfileName = ".citadel-session-pids"

// CobrowseSessionState is the lifecycle state of one session, reported by status.
type CobrowseSessionState string

const (
	// SessionLaunching: the session slot is reserved and the browser is starting
	// (Xvfb + Chromium spawning, waiting for CDP). Observable because the slot is
	// registered before the launch completes.
	SessionLaunching CobrowseSessionState = "launching"
	// SessionRunning: the browser is up and its CDP endpoint is ready.
	SessionRunning CobrowseSessionState = "running"
	// SessionAttached: a viewer/human is attached to the session (set via
	// MarkAttached; the screencast + input bridge, issue #794, calls it).
	SessionAttached CobrowseSessionState = "attached"
	// SessionExited: the browser process terminated (crashed or was killed) while
	// the session was still tracked. Terminal; the session should be stopped/pruned.
	SessionExited CobrowseSessionState = "exited"
)

// CobrowseSessionStatus is the queryable state of one session.
type CobrowseSessionStatus struct {
	ID    string               `json:"id"`
	State CobrowseSessionState `json:"state"`
	URL   string               `json:"url,omitempty"`
	// Title is the page title after a navigate (bounded, control characters
	// stripped); empty for every other status.
	Title     string `json:"title,omitempty"`
	DebugPort int    `json:"debug_port,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Display   string `json:"display,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	// Driver reports human only after explicit handoff. An attached viewer
	// without handoff observes while the agent continues to act.
	Driver CobrowseDriver `json:"driver,omitempty"`
	// Browser is the launched browser's kind ("chrome", "edge", "chromium").
	Browser string `json:"browser,omitempty"`
	// Generation counts ownership changes (first launch is 1, each re-adopt adds 1).
	Generation uint64 `json:"generation,omitempty"`
}

// cobrowseProc is the running-process bundle for one session: the browser and its
// Xvfb, their PIDs (for the crash-recovery pidfile), a channel closed when the
// browser exits, and a bounded teardown closure. Returned by launchCobrowseProc so
// the spawn is injectable in tests without launching a real browser.
type cobrowseProc struct {
	debugPort int
	// browserID is the browser's per-launch CDP id, learned once the endpoint is up.
	browserID string
	display   string
	// browser is the launched browser's kind ("chrome", "edge", "chromium"),
	// reported in status so a user can see which installed browser was used.
	browser    string
	browserPID int
	xvfbPID    int
	// browserScope and xvfbScope name the transient systemd scopes the processes
	// run in (empty when launched directly); scopeUser says which manager owns them.
	browserScope, xvfbScope string
	scopeUser               bool
	// exited is closed by the reaper when the browser process terminates, so status
	// can report SessionExited for a crashed session rather than a bare running=false.
	exited <-chan struct{}
	// stop kills the browser then the Xvfb (bounded waits) and is safe to call once.
	stop func() error
}

// launchCobrowseProc launches an isolated headless browser on a fresh dedicated
// virtual display with the given throwaway profile dir. It is a package var so tests
// can substitute a deterministic fake (mirroring portOwnerLookup / pidKiller). It
// blocks until CDP is ready so the returned session is immediately drivable.
// Display allocation is serialized inside startManagedXvfb, so concurrent launches
// never collide on one virtual display.
var launchCobrowseProc = defaultLaunchCobrowseProc

// SetLaunchOptions sets the launch options for this manager's future
// sessions. Local mode sets a guarding proxy and quiet mode.
func (m *CobrowseSessionManager) SetLaunchOptions(lo LaunchOptions) {
	m.mu.Lock()
	m.launchOpts = lo
	m.mu.Unlock()
}

// CobrowseSessionManager owns the map of concurrent, isolated browser sessions. Safe
// for concurrent use.
type CobrowseSessionManager struct {
	mu          sync.Mutex
	sessions    map[string]*cobrowseSession
	maxSessions int
	baseDir     string
	sweepOnce   sync.Once
	launchOpts  LaunchOptions
	// useResolver is set on the process-wide manager only, so its registry
	// follows registryDirResolver; other managers keep a sibling of baseDir.
	useResolver bool
}

// SessionProfile is an encrypted, PIN-unlocked persistent profile bound to one
// session (issue #795). It is satisfied structurally by internal/cobrowseprofile's
// Handle; declaring it as an interface here keeps platform free of a nodevault /
// network import (network imports platform, so a back-import would cycle). The
// caller unlocks the profile with the PIN BEFORE StartSessionWithProfile and hands
// over ownership; the manager guarantees Close eventually runs on every path.
//
//   - Materialize decrypts the stored profile into the session's private working
//     dir (already created 0700, empty) before the browser launches; first use
//     leaves it empty for a fresh profile. It must fail closed.
//   - Persist re-encrypts that working dir back to the store. The manager calls it
//     ONLY after the browser actually ran, so a failed launch never overwrites a
//     good profile with an empty dir.
//   - Close zeroes the unlocked key material and releases the profile's
//     single-session lock. Idempotent.
type SessionProfile interface {
	Materialize(dir string) error
	Persist(dir string) error
	Close() error
}

// cobrowseSession is one isolated browser session. Its own mutex guards proc/state so
// a status/stop of a sibling never blocks on this session's launch or CDP wait.
type cobrowseSession struct {
	id        string
	profile   string
	startedAt time.Time

	// encProfile is the encrypted persistent profile for this session, or nil for a
	// throwaway session. When set, teardown persists (only if the browser ran) and
	// always Closes it.
	encProfile SessionProfile

	mu    sync.Mutex
	state CobrowseSessionState
	proc  *cobrowseProc
	// explicitHandoff blocks agent actions and enables viewer input until
	// resume, even if the viewer temporarily disconnects.
	explicitHandoff bool
	// A recording owns the page only until its bounded capture is published or
	// cancelled. Stop, handoff and viewer attachment cancel it under mu, then
	// wait on recordDone without holding mu.
	recordCancel context.CancelFunc
	recordDone   chan struct{}
	// stopping closes the admission gap after the manager removes this session
	// from its map but before teardown clears proc.
	stopping bool
	// persistent marks a session launched with an encrypted profile. A
	// re-adopted one has lost its unlocked key, so teardown cannot re-seal it.
	persistent bool
	// regDir is the registry dir holding this session's record.
	regDir     string
	generation uint64
	pendingEnd *EndedSession
}

var (
	cobrowseSessionManager     *CobrowseSessionManager
	cobrowseSessionManagerOnce sync.Once
)

// GetCobrowseSessionManager returns the process-wide multi-session manager singleton,
// mirroring GetCobrowseManager(). Sessions persist across jobs in this one manager.
func GetCobrowseSessionManager() *CobrowseSessionManager {
	cobrowseSessionManagerOnce.Do(func() {
		cobrowseSessionManager = newCobrowseSessionManager(
			defaultCobrowseSessionsBaseDir(),
			resolveMaxCobrowseSessions(),
		)
		cobrowseSessionManager.useResolver = true
	})
	return cobrowseSessionManager
}

// NewCobrowseSessionManager returns an isolated manager (not the process-wide
// singleton) rooted at baseDir with its own session cap. Local mode
// (internal/localcobrowse) uses it so its stricter cap does not depend on
// EnvCobrowseMaxSessions.
func NewCobrowseSessionManager(baseDir string, maxSessions int) *CobrowseSessionManager {
	return newCobrowseSessionManager(baseDir, maxSessions)
}

// DefaultCobrowseSessionsBaseDir is the per-OS parent of session working dirs.
func DefaultCobrowseSessionsBaseDir() string { return defaultCobrowseSessionsBaseDir() }

// newCobrowseSessionManager constructs a manager rooted at baseDir with the given
// cap. Split out so tests get an isolated manager (own temp baseDir) instead of
// mutating the process-wide singleton and clobbering the operator's real sessions.
func newCobrowseSessionManager(baseDir string, maxSessions int) *CobrowseSessionManager {
	if maxSessions <= 0 {
		maxSessions = defaultMaxCobrowseSessions
	}
	return &CobrowseSessionManager{
		sessions:    make(map[string]*cobrowseSession),
		maxSessions: maxSessions,
		baseDir:     baseDir,
	}
}

// resolveMaxCobrowseSessions reads EnvCobrowseMaxSessions, falling back to the default
// when unset or not a positive integer.
func resolveMaxCobrowseSessions() int {
	if v := strings.TrimSpace(os.Getenv(EnvCobrowseMaxSessions)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxCobrowseSessions
}

// newSessionID returns a short random hex session handle.
func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a time-based id; collision is astronomically unlikely and a
		// duplicate would only reuse a dir, not corrupt another live session.
		return fmt.Sprintf("cb-%d", time.Now().UnixNano())
	}
	return "cb-" + hex.EncodeToString(b[:])
}

// StartSession launches a new isolated browser session and returns its handle. It
// blocks until the browser's CDP endpoint is ready (like the sibling launchers), then
// reports the running session. The session is registered as SessionLaunching before
// the launch so it is observable and reapable mid-launch by StopAll on shutdown.
func (m *CobrowseSessionManager) StartSession(startURL string) (CobrowseSessionStatus, error) {
	return m.StartSessionWithProfile(startURL, nil)
}

// StartSessionWithProfile is StartSession with an optional encrypted persistent
// profile (issue #795). When profile is nil it is exactly StartSession: a throwaway
// working dir removed on stop. When non-nil, the caller has already unlocked the
// profile with the PIN; the working dir is materialized from it before launch and
// re-encrypted on stop. Ownership of profile transfers to the manager: Close is
// guaranteed to run on every exit path here (failed launch, stopped-during-launch)
// and on teardown, so neither the unlocked key nor the profile's single-session
// lock can leak — a missed Close is structurally impossible because every cleanup
// path routes through teardown().
func (m *CobrowseSessionManager) StartSessionWithProfile(startURL string, profile SessionProfile) (status CobrowseSessionStatus, err error) {
	return m.startSession(newSessionID(), startURL, profile)
}

// NewCobrowseSessionID returns a fresh session id in the manager's format. A
// caller that must hand out an id before the browser exists (local mode's
// locked persistent-profile session) reserves one here and passes it to
// StartSessionWithID once the profile is unlocked.
func NewCobrowseSessionID() string { return newSessionID() }

// reservedSessionIDPattern is the shape newSessionID produces.
var reservedSessionIDPattern = regexp.MustCompile(`^cb-[0-9a-f]{16}$`)

// StartSessionWithID is StartSessionWithProfile under a caller-reserved id from
// NewCobrowseSessionID. An id of any other shape, or one already in use, is
// refused (and the profile Closed), so a caller cannot name a directory.
func (m *CobrowseSessionManager) StartSessionWithID(id, startURL string, profile SessionProfile) (CobrowseSessionStatus, error) {
	if !reservedSessionIDPattern.MatchString(id) {
		if profile != nil {
			_ = profile.Close()
		}
		return CobrowseSessionStatus{}, fmt.Errorf("invalid co-browse session id %q", id)
	}
	return m.startSession(id, startURL, profile)
}

func (m *CobrowseSessionManager) startSession(id, startURL string, profile SessionProfile) (status CobrowseSessionStatus, err error) {
	if err := ValidateCobrowseURL(startURL); err != nil {
		if profile != nil {
			profile.Close()
		}
		return CobrowseSessionStatus{}, err
	}
	// Reap orphans from a prior crashed process before the first launch of this one.
	m.sweepOnce.Do(m.sweep)

	m.mu.Lock()
	if len(m.sessions) >= m.maxSessions {
		max := m.maxSessions
		m.mu.Unlock()
		// The caller handed us the profile; Close it since we are not taking it.
		if profile != nil {
			_ = profile.Close()
		}
		return CobrowseSessionStatus{}, fmt.Errorf(
			"too many co-browse sessions (%d running); stop one first or raise %s",
			max, EnvCobrowseMaxSessions)
	}
	if _, taken := m.sessions[id]; taken {
		m.mu.Unlock()
		if profile != nil {
			_ = profile.Close()
		}
		return CobrowseSessionStatus{}, fmt.Errorf("co-browse session %s already exists", id)
	}
	s := &cobrowseSession{
		id:         id,
		profile:    filepath.Join(m.baseDir, id),
		startedAt:  time.Now(),
		state:      SessionLaunching,
		encProfile: profile,
		regDir:     m.sessionRegDir(),
		generation: 1,
	}
	m.sessions[id] = s
	m.mu.Unlock()

	proc, err := m.launch(s, startURL)
	if err != nil {
		// Failed launch: drop the reserved slot and tear the session down. teardown
		// removes the working dir and Closes the profile (no Persist, since proc is
		// nil), so a launch that never produced a browser leaks neither a slot, a
		// dir, nor an unlocked key.
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		_ = s.teardown()
		return CobrowseSessionStatus{}, err
	}

	// A concurrent Stop/StopAll (e.g. node shutdown mid-launch) may have removed this
	// session from the map while the browser was still starting. teardown() then saw
	// proc==nil and only removed the profile dir (and Closed the profile), so the
	// browser we just finished launching is owned by nobody -- not the map, not a
	// pidfile. Re-check under the lock: if the slot is gone, tear down the fresh
	// browser ourselves so it does not orphan across shutdown. Close on the profile
	// is idempotent, so a double Close across the two teardowns is safe.
	m.mu.Lock()
	if m.sessions[id] != s {
		m.mu.Unlock()
		_ = proc.stop()
		// Do NOT persist here: the session was torn down mid-launch, the concurrent
		// teardown may already have removed the working dir, and sealing an empty or
		// vanishing dir would overwrite a good profile with nothing. Just Close
		// (idempotent — the concurrent teardown already Closed it) and remove the dir.
		if profile != nil {
			_ = profile.Close()
		}
		_ = os.RemoveAll(s.profile)
		deleteRecord(s.regDir, id)
		return CobrowseSessionStatus{}, fmt.Errorf("session %s was stopped during launch", id)
	}
	m.mu.Unlock()

	st := s.status()
	if startURL != "" && !strings.EqualFold(startURL, "about:blank") {
		// Wait for the launch URL (including redirects) to settle, then report
		// where the page ended up, as Navigate does. Best effort: a page that
		// cannot be read leaves the plain status.
		if port, perr := s.requireDrivablePort(); perr == nil {
			ps := settleNavigation(port, "")
			if ps.URL != "" {
				st.URL = ps.URL
			}
			st.Title = ps.Title
		}
	}
	return st, nil
}

// launch creates the session's profile dir, spawns the browser via the (injectable)
// launcher, records the PIDs for crash recovery, and flips the session to running.
// Returns the running proc so the caller can tear it down if the session was stopped
// during launch.
func (m *CobrowseSessionManager) launch(s *cobrowseSession, startURL string) (*cobrowseProc, error) {
	// A runner that needs a trusted, private parent (Windows: owner-only DACL)
	// refuses here before any profile data is written beneath it.
	if err := activeBrowserRunner.PrepareBaseDir(m.baseDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.profile, 0o700); err != nil {
		return nil, fmt.Errorf("create session profile dir: %w", err)
	}
	// For a persistent session, decrypt the stored profile into the (private, 0700)
	// working dir BEFORE launching the browser, so the browser opens with the saved
	// logins. Fails closed: a materialize error (e.g. tampered/corrupt store) aborts
	// the launch rather than starting with a partial profile. Read encProfile under
	// s.mu: a concurrent teardown (StopAll mid-launch) nils it under the same lock.
	s.mu.Lock()
	enc := s.encProfile
	s.mu.Unlock()
	if enc != nil {
		// Mark the dir before any plaintext lands in it, so an interrupted
		// session can be traced to its profile and cleaned up.
		if err := writeProfileMarker(s.profile, enc); err != nil {
			return nil, fmt.Errorf("mark encrypted profile working dir: %w", err)
		}
		if err := enc.Materialize(s.profile); err != nil {
			return nil, fmt.Errorf("materialize encrypted profile: %w", err)
		}
	}
	m.mu.Lock()
	lo := m.launchOpts
	m.mu.Unlock()
	lo.Detach = m.useResolver && detachedWorker.Load()
	proc, err := launchCobrowseProc(s.profile, startURL, lo)
	if err != nil {
		return nil, err
	}
	// Best-effort pidfile so a future process can reap this browser + Xvfb if THIS
	// process is SIGKILLed before graceful teardown. A write failure is non-fatal:
	// the session still runs and graceful stop still reaps it in-process.
	writeSessionPidfile(s.profile, os.Getpid(), proc.browserPID, proc.xvfbPID, currentProcessStartToken())

	s.mu.Lock()
	s.proc = proc
	s.state = SessionRunning
	s.persistent = enc != nil
	s.mu.Unlock()
	// Durable registry record so a restart by exec can re-adopt this session.
	s.persistRecord()
	return proc, nil
}

// SessionStatus returns the queryable state of one session.
func (m *CobrowseSessionManager) SessionStatus(id string) (CobrowseSessionStatus, bool) {
	m.Recover()
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return CobrowseSessionStatus{}, false
	}
	return s.status(), true
}

// List returns the status of every tracked session.
func (m *CobrowseSessionManager) List() []CobrowseSessionStatus {
	m.Recover()
	m.mu.Lock()
	all := make([]*cobrowseSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	out := make([]CobrowseSessionStatus, 0, len(all))
	for _, s := range all {
		out = append(out, s.status())
	}
	return out
}

// MarkAttached flips a session to the attached state (a viewer/human is watching).
// The screencast + input bridge (issue #794) calls this; it is a no-op hook here that
// only records the state so status reflects it. Returns false for an unknown session.
func (m *CobrowseSessionManager) MarkAttached(id string) bool { return m.setAttached(id, true) }

// MarkDetached returns an attached session to the running state.
func (m *CobrowseSessionManager) MarkDetached(id string) bool { return m.setAttached(id, false) }

func (m *CobrowseSessionManager) setAttached(id string, attached bool) bool {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return false
	}
	s.mu.Lock()
	// Never override a terminal/launching state: attach only makes sense once running.
	var done <-chan struct{}
	if s.state == SessionRunning && attached {
		s.state = SessionAttached
		done = s.cancelRecordingLocked()
	} else if s.state == SessionAttached && !attached {
		s.state = SessionRunning
	}
	s.mu.Unlock()
	if done != nil {
		<-done
	}
	return true
}

// cancelRecordingLocked signals a live capture without waiting under s.mu.
// The caller waits for done before teardown or handing control to a viewer.
func (s *cobrowseSession) cancelRecordingLocked() <-chan struct{} {
	if s.recordCancel != nil {
		s.recordCancel()
	}
	return s.recordDone
}

func (s *cobrowseSession) beginStoppingRecording() <-chan struct{} {
	s.mu.Lock()
	s.stopping = true
	done := s.cancelRecordingLocked()
	s.mu.Unlock()
	return done
}

func waitRecording(done <-chan struct{}) {
	if done != nil {
		<-done
	}
}

// Stop tears down one session (browser + Xvfb) and removes its throwaway profile dir.
// Safe to call for an unknown id (returns nil) so a double-stop is not an error.
func (m *CobrowseSessionManager) Stop(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if !ok {
		return nil
	}
	waitRecording(s.beginStoppingRecording())
	return s.teardown()
}

// StopAll tears down every session. Called on node shutdown so no browser or virtual
// display orphans across a worker restart. Best-effort: a failure on one session does
// not skip the rest; the last error is returned.
func (m *CobrowseSessionManager) StopAll() error {
	m.mu.Lock()
	all := m.sessions
	m.sessions = make(map[string]*cobrowseSession)
	m.mu.Unlock()

	// Mark every session terminal and signal every capture before waiting for
	// any one encoder to finish.
	type stoppingSession struct {
		s    *cobrowseSession
		done <-chan struct{}
	}
	pending := make([]stoppingSession, 0, len(all))
	for _, s := range all {
		pending = append(pending, stoppingSession{s: s, done: s.beginStoppingRecording()})
	}
	var lastErr error
	for _, item := range pending {
		waitRecording(item.done)
		if err := item.s.teardown(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// teardown kills the session's processes (if launched), persists the encrypted
// profile (only if the browser actually ran), Closes the profile, and removes the
// plaintext working dir. Every cleanup path routes through here, so the profile is
// always Closed exactly-effectively-once (Close is idempotent) — the unlocked key
// and the single-session lock cannot leak.
func (s *cobrowseSession) teardown() error {
	s.mu.Lock()
	proc := s.proc
	s.proc = nil
	profile := s.profile
	enc := s.encProfile
	s.encProfile = nil
	regDir := s.regDir
	s.mu.Unlock()
	// A stopped session leaves no registry record: it did not "end" unexpectedly.
	if regDir != "" {
		deleteRecord(regDir, s.id)
	}

	var err error
	if enc != nil && proc != nil {
		// A persistent profile is sealed from what the browser wrote to disk.
		// Chromium keeps recent cookie and login writes in memory and commits
		// them in batches, so a hard kill right after a login would seal a
		// profile without it. Ask the browser to close first (bounded), then
		// kill whatever is left as before. Throwaway sessions skip this.
		closeBrowserGracefully(proc)
	}
	if proc != nil && proc.stop != nil {
		err = proc.stop()
	}
	if enc != nil {
		// Persist ONLY if the browser ran (proc != nil): a failed launch never
		// produced new state, and sealing an empty/partial working dir would
		// overwrite a good profile with nothing.
		if proc != nil {
			if perr := enc.Persist(profile); perr != nil && err == nil {
				err = perr
			}
		}
		// Close zeroes the unlocked key material and releases the profile's
		// single-session lock, always — even when Persist failed above.
		_ = enc.Close()
	}
	// Remove the plaintext working dir so a stopped session leaves no decrypted
	// cookies (and no throwaway state) behind. The pidfile lives under it and goes
	// too. For a persistent profile the durable state now lives only as ciphertext.
	if profile != "" {
		if rerr := removeSessionDir(profile); rerr != nil && enc != nil && err == nil {
			// A persistent session's working dir holds decrypted cookies; a
			// failure to remove it must be visible, never silent.
			err = fmt.Errorf("the decrypted working copy of a saved profile could not be removed from %s: %w", profile, rerr)
		}
	}
	return err
}

// status computes the live queryable state, detecting a crashed browser via the
// reaper channel so a dead session reports SessionExited rather than a stale running.
//
// The CDP probe (pickTarget) runs OUTSIDE s.mu: it is an HTTP round-trip that can take
// up to a few seconds against a wedged endpoint, and teardown() also takes s.mu -- so
// probing under the lock would let a hung CDP endpoint add that latency to every
// Stop/StopAll of this session (and, since List calls status() per session serially,
// to every session status in the list). st is a local, unshared struct, so filling in
// its URL after unlocking is race-free.
func (s *cobrowseSession) status() CobrowseSessionStatus {
	s.mu.Lock()
	st := CobrowseSessionStatus{
		ID:         s.id,
		State:      s.state,
		Profile:    s.profile,
		Driver:     driverFor(s.humanDrivingLocked()),
		Generation: s.generation,
	}
	if !s.startedAt.IsZero() {
		st.StartedAt = s.startedAt.UTC().Format(time.RFC3339)
	}
	var probePort int
	if s.proc != nil {
		st.DebugPort = s.proc.debugPort
		st.Display = s.proc.display
		st.Browser = s.proc.browser
		select {
		case <-s.proc.exited:
			// The browser died while we still track it: terminal state.
			st.State = SessionExited
		default:
		}
		if st.State == SessionRunning || st.State == SessionAttached {
			probePort = s.proc.debugPort
		}
	}
	s.mu.Unlock()

	if probePort != 0 {
		if t, err := pickTarget(probePort); err == nil {
			st.URL = t.URL
		}
	}
	return st
}

// defaultLaunchCobrowseProc launches through this OS's browser runner (see
// cobrowse_runner.go): the managed-Xvfb runner on Linux, the headless Job
// Object runner on Windows.
func defaultLaunchCobrowseProc(profileDir, startURL string, lo LaunchOptions) (*cobrowseProc, error) {
	// Every runner (Xvfb, Windows headless) is reached only through here, so
	// the start-URL gate holds even for a caller that skips the manager's.
	if err := ValidateCobrowseURL(startURL); err != nil {
		return nil, err
	}
	return activeBrowserRunner.Launch(profileDir, startURL, lo)
}

// stopBrowserProc kills the browser first, then the Xvfb (so the browser is never
// left without a display mid-shutdown), each with a bounded wait on its reaper so a
// hung child cannot wedge shutdown. Safe to call once.
func stopBrowserProc(cmd *exec.Cmd, exited <-chan struct{}, xvfb *exec.Cmd, xvfbExited <-chan struct{}) error {
	var firstErr error
	if cmd != nil && cmd.Process != nil {
		if err := cmd.Process.Kill(); err != nil && !isProcessGoneErr(err) {
			firstErr = fmt.Errorf("kill browser: %w", err)
		}
		waitBounded(exited)
	}
	if xvfb != nil && xvfb.Process != nil {
		if err := xvfb.Process.Kill(); err != nil && !isProcessGoneErr(err) && firstErr == nil {
			firstErr = fmt.Errorf("kill Xvfb: %w", err)
		}
		waitBounded(xvfbExited)
	}
	return firstErr
}

// waitBounded waits for ch or a 5s ceiling, whichever first, so teardown never blocks
// indefinitely on a child that refuses to die.
func waitBounded(ch <-chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
	}
}

// pidOf returns a started command's PID, or 0 when it has no process handle.
func pidOf(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

// writeSessionPidfile records the OWNING citadel process PID plus the browser and
// Xvfb PIDs under the session dir, so a later process's sweep can reap the browser +
// Xvfb if the owner dies before graceful teardown -- while leaving sessions of a
// still-live sibling process (e.g. the control-center worker running beside
// `citadel work`) untouched.
//
// ownerStart is an OS-specific identity token for the owner process (on Windows,
// its creation time) so a later sweep can tell the recorded owner apart from an
// unrelated process that reused its PID. Zero means "not recorded" and keeps the
// historical three-line format byte-for-byte.
func writeSessionPidfile(profileDir string, ownerPID, browserPID, xvfbPID int, ownerStart int64) {
	path := filepath.Join(profileDir, sessionPidfileName)
	content := fmt.Sprintf("%d\n%d\n%d\n", ownerPID, browserPID, xvfbPID)
	if ownerStart != 0 {
		content += fmt.Sprintf("%d\n", ownerStart)
	}
	_ = os.WriteFile(path, []byte(content), 0o600)
}

// readSessionPidfile parses the owner, browser, and Xvfb PIDs from a session dir's
// pidfile. Missing or malformed lines yield 0 (skip), never an error, so a partial
// file still lets the sweep reap whatever it can.
func readSessionPidfile(profileDir string) (ownerPID, browserPID, xvfbPID int) {
	data, err := os.ReadFile(filepath.Join(profileDir, sessionPidfileName))
	if err != nil {
		return 0, 0, 0
	}
	lines := strings.Fields(string(data))
	if len(lines) > 0 {
		ownerPID, _ = strconv.Atoi(lines[0])
	}
	if len(lines) > 1 {
		browserPID, _ = strconv.Atoi(lines[1])
	}
	if len(lines) > 2 {
		xvfbPID, _ = strconv.Atoi(lines[2])
	}
	return ownerPID, browserPID, xvfbPID
}

// readSessionOwnerStart returns the owner identity token from the pidfile's
// optional fourth line, or 0 when absent or malformed.
func readSessionOwnerStart(profileDir string) int64 {
	data, err := os.ReadFile(filepath.Join(profileDir, sessionPidfileName))
	if err != nil {
		return 0
	}
	lines := strings.Fields(string(data))
	if len(lines) < 4 {
		return 0
	}
	v, err := strconv.ParseInt(lines[3], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// ensureCobrowseBaseDir makes sure the session base dir exists, is owned by the
// current user, and is not group/other-writable -- creating it (0o700) when absent so
// the normal, no-prior-state path always passes. sweep() must call this BEFORE it
// trusts any pidfile found under the dir: the parent lives under a shared, world-
// writable os.TempDir(), so without this check a local unprivileged user could
// pre-create the dir (or leave a stale one group/other-writable) and plant a pidfile
// naming an arbitrary PID for a future sweep -- often running as root under systemd,
// see CLAUDE.md's worker-watchdog notes -- to kill. Deliberately never modifies an
// EXISTING dir that fails the check (no auto-chmod/chown): the caller refuses to sweep
// instead.
func ensureCobrowseBaseDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			// Windows: replace the inherited ACL with an explicit owner-only DACL.
			// A dir that cannot be secured is removed again so the next call
			// does not trust an inherited, possibly shared ACL. No-op on Unix,
			// where MkdirAll's 0o700 already is the owner-only guarantee.
			if err := secureNewCobrowseBaseDir(dir); err != nil {
				_ = os.Remove(dir)
				return err
			}
			// MkdirAll also succeeds if another process created the dir in
			// the meantime, with whatever mode or owner it chose, so check
			// what is actually there before trusting it.
			info, err = os.Lstat(dir)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("cobrowse base dir %s exists and is not a directory", dir)
			}
			return validateCobrowseBaseDirPerms(dir, info)
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("cobrowse base dir %s exists and is not a directory", dir)
	}
	return validateCobrowseBaseDirPerms(dir, info)
}

// sweep reaps sessions orphaned by a CRASHED/SIGKILLed process: for every session dir
// under baseDir whose owning citadel process is no longer alive, it identity-verifies
// and kills the recorded browser + Xvfb PIDs, then removes the dir. Dirs owned by a
// still-live process are left alone -- multiple citadel processes on one host (e.g.
// `citadel work` beside the control-center worker) share this parent dir, and one must
// never reap another's live sessions. Runs once, before this process's first
// StartSession. Reuses the injectable pidKiller / pidAlive seams from
// cobrowse_orphan.go plus processMatchesRole from cobrowse_identity.go.
//
// SECURITY: a pidfile is an on-disk claim from a PRIOR process, not a guarantee -- PIDs
// are recycled by the OS, this base dir survives a reboot (plain /tmp, often not
// tmpfs), and the recorded PID could by now belong to an unrelated process (sshd, a
// user shell, ...). Two independent gates protect the kill:
//  1. ensureCobrowseBaseDir, above, confirms the pidfiles themselves are trustworthy
//     (nobody else could have planted this directory).
//  2. processMatchesRole confirms, PID by PID, that the specific PID we are about to
//     SIGKILL is STILL the browser/Xvfb it was recorded as, via /proc/<pid>/cmdline.
//     A PID that fails this check is never killed -- only the stale dir is cleaned up.
func (m *CobrowseSessionManager) sweep() {
	// Re-adopt sessions that survived a restart BEFORE the sweep, which would
	// otherwise reap them as orphans (citadel-cli#1546).
	m.recoverSessions()
	activeBrowserRunner.SweepOrphans(m.baseDir)
}

// sweepXvfbSessions is the Linux runner's orphan sweep; see sweep above.
func sweepXvfbSessions(baseDir string) {
	if err := ensureCobrowseBaseDir(baseDir); err != nil {
		log.Printf("[cobrowse] refusing session sweep of %s: %v", baseDir, err)
		return
	}
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return // no parent dir yet: nothing to reap
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(baseDir, e.Name())
		if _, keep := sweepProtected.Load(dir); keep {
			continue // re-adopted by this process
		}
		ownerPID, browserPID, xvfbPID := readSessionPidfile(dir)
		// Skip a dir whose owning process is still alive: it is not an orphan, and its
		// browser belongs to a live sibling worker.
		if ownerPID > 0 && ownerPID != os.Getpid() && pidAlive(ownerPID) {
			continue
		}
		sweepKillIfVerified(browserPID, roleCobrowseBrowser, "browser")
		sweepKillIfVerified(xvfbPID, roleCobrowseXvfb, "Xvfb")
		_ = os.RemoveAll(dir)
	}
}

// sweepKillIfVerified kills pid only when processMatchesRole confirms it is still the
// process sweep recorded it as for role. A mismatch or unverifiable PID is logged and
// left alone -- fail closed, never kill on doubt.
func sweepKillIfVerified(pid int, role cobrowseProcRole, label string) {
	if pid <= 0 || pid == os.Getpid() {
		return
	}
	if !processMatchesRole(pid, role) {
		log.Printf("[cobrowse] sweep: pid %d recorded as %s no longer matches (or is unverifiable); skipping kill", pid, label)
		return
	}
	_ = pidKiller(pid)
}

// EnsurePrivateDir creates dir private to the current user, or verifies an
// existing one, with the same rules as the co-browse session base dir: owner
// only and not group/other-writable on Unix; owned by the user with a
// protected owner-only DACL on Windows. An existing dir that fails is refused,
// never repaired. Local mode keeps its viewer token and audit log in such dirs.
func EnsurePrivateDir(dir string) error { return ensureCobrowseBaseDir(dir) }

// gracefulCloseWait is the whole budget for closing a persistent session's
// browser gracefully: the endpoint probe, the Browser.close call and the wait
// for the process to exit all share it, after which teardown kills as usual.
// A var for tests.
var gracefulCloseWait = 10 * time.Second

// closeBrowserGracefully sends CDP Browser.close to proc's browser target and
// waits, within gracefulCloseWait in total, for the process to exit, so
// Chromium flushes its cookie and login stores to disk. Best effort: no debug
// port, an unreadable or mismatched endpoint, or a browser that does not exit
// in time all fall through to the caller's kill.
func closeBrowserGracefully(proc *cobrowseProc) {
	if proc == nil || proc.debugPort <= 0 || proc.exited == nil {
		return
	}
	select {
	case <-proc.exited:
		return
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), gracefulCloseWait)
	defer cancel()
	ws, err := fetchCDPBrowserWSContext(ctx, proc.debugPort)
	if err != nil {
		return
	}
	// Only close the browser this session launched, on the port it owns.
	prefix := fmt.Sprintf("ws://127.0.0.1:%d/devtools/browser/", proc.debugPort)
	if !strings.HasPrefix(ws, prefix) || (proc.browserID != "" && ws != prefix+proc.browserID) {
		return
	}
	// The browser may close the socket before replying; that is success.
	_, _ = cdpDialAndSendContext(ctx, ws, "Browser.close", nil)
	select {
	case <-proc.exited:
	case <-ctx.Done():
	}
}
