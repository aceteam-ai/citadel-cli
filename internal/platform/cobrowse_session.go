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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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
	ID        string               `json:"id"`
	State     CobrowseSessionState `json:"state"`
	URL       string               `json:"url,omitempty"`
	DebugPort int                  `json:"debug_port,omitempty"`
	Profile   string               `json:"profile,omitempty"`
	Display   string               `json:"display,omitempty"`
	StartedAt string               `json:"started_at,omitempty"`
	// Driver is the session's driver-arbitration state (issue #978): "ai" while
	// the agent may issue scripted actions (navigate/click/type/screenshot/
	// extract), "human" while those are refused (ErrHandedOff). Reuses the same
	// CobrowseDriver type and DriverAI/DriverHuman values as the singleton
	// CobrowseManager (cobrowse.go) -- same concept, per-session instead of
	// per-node.
	Driver CobrowseDriver `json:"driver,omitempty"`
}

// cobrowseProc is the running-process bundle for one session: the browser and its
// Xvfb, their PIDs (for the crash-recovery pidfile), a channel closed when the
// browser exits, and a bounded teardown closure. Returned by launchCobrowseProc so
// the spawn is injectable in tests without launching a real browser.
type cobrowseProc struct {
	debugPort  int
	display    string
	browserPID int
	xvfbPID    int
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

// CobrowseSessionManager owns the map of concurrent, isolated browser sessions. Safe
// for concurrent use.
type CobrowseSessionManager struct {
	mu          sync.Mutex
	sessions    map[string]*cobrowseSession
	maxSessions int
	baseDir     string
	sweepOnce   sync.Once
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
	// driver is this session's driver-arbitration state (issue #978): DriverAI
	// (default) while the agent may issue scripted navigate/click/type/
	// screenshot/extract actions, DriverHuman while those are refused
	// (ErrHandedOff). Flipped explicitly by Handoff/Resume, and implicitly by
	// MarkAttached/MarkDetached -- see setAttached for the interop rule.
	driver CobrowseDriver
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
			filepath.Join(os.TempDir(), cobrowseSessionsDirName),
			resolveMaxCobrowseSessions(),
		)
	})
	return cobrowseSessionManager
}

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
	id := newSessionID()
	s := &cobrowseSession{
		id:         id,
		profile:    filepath.Join(m.baseDir, id),
		startedAt:  time.Now(),
		state:      SessionLaunching,
		encProfile: profile,
		driver:     DriverAI,
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
		return CobrowseSessionStatus{}, fmt.Errorf("session %s was stopped during launch", id)
	}
	m.mu.Unlock()

	return s.status(), nil
}

// launch creates the session's profile dir, spawns the browser via the (injectable)
// launcher, records the PIDs for crash recovery, and flips the session to running.
// Returns the running proc so the caller can tear it down if the session was stopped
// during launch.
func (m *CobrowseSessionManager) launch(s *cobrowseSession, startURL string) (*cobrowseProc, error) {
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
		if err := enc.Materialize(s.profile); err != nil {
			return nil, fmt.Errorf("materialize encrypted profile: %w", err)
		}
	}
	proc, err := launchCobrowseProc(s.profile, startURL)
	if err != nil {
		return nil, err
	}
	// Best-effort pidfile so a future process can reap this browser + Xvfb if THIS
	// process is SIGKILLed before graceful teardown. A write failure is non-fatal:
	// the session still runs and graceful stop still reaps it in-process.
	writeSessionPidfile(s.profile, os.Getpid(), proc.browserPID, proc.xvfbPID)

	s.mu.Lock()
	s.proc = proc
	s.state = SessionRunning
	s.mu.Unlock()
	return proc, nil
}

// SessionStatus returns the queryable state of one session.
func (m *CobrowseSessionManager) SessionStatus(id string) (CobrowseSessionStatus, bool) {
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

// setAttached is also the driver-arbitration interop point (issue #978): a
// viewer attaching hands the session's driver to the human, so an in-flight or
// subsequent agent-scripted action (navigate/click/type/screenshot/extract) is
// refused (ErrHandedOff) the moment a human is watching, without requiring a
// separate explicit `handoff` job call -- "a human can grab a live scripted
// session mid-run". Detaching deliberately does NOT hand the driver back to
// the AI: a transient viewer disconnect (network blip, tab close) must not
// silently resume agent scripting on a session the human may still consider
// theirs (e.g. mid-2FA). Returning control to the agent requires the explicit
// `resume` action, mirroring the old singleton's explicit Resume() contract.
func (m *CobrowseSessionManager) setAttached(id string, attached bool) bool {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Never override a terminal/launching state: attach only makes sense once running.
	if s.state == SessionRunning && attached {
		s.state = SessionAttached
		s.driver = DriverHuman
	} else if s.state == SessionAttached && !attached {
		s.state = SessionRunning
	}
	return true
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

	var lastErr error
	for _, s := range all {
		if err := s.teardown(); err != nil {
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
	s.mu.Unlock()

	var err error
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
		_ = os.RemoveAll(profile)
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
		ID:      s.id,
		State:   s.state,
		Profile: s.profile,
		Driver:  s.driver,
	}
	if !s.startedAt.IsZero() {
		st.StartedAt = s.startedAt.UTC().Format(time.RFC3339)
	}
	var probePort int
	if s.proc != nil {
		st.DebugPort = s.proc.debugPort
		st.Display = s.proc.display
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

// defaultLaunchCobrowseProc is the real browser launcher: a headless-by-default
// Chromium on a fresh dedicated Xvfb display with an isolated profile and CDP port.
// Mirrors MeetingBrowser.Start's teardown-on-partial-failure, but the profile is
// throwaway so a failed launch removal is handled by the caller.
func defaultLaunchCobrowseProc(profileDir, startURL string) (*cobrowseProc, error) {
	chrome, err := findChromium()
	if err != nil {
		return nil, err
	}

	debugPort, err := findFreeDebugPort()
	if err != nil {
		return nil, err
	}

	// startManagedXvfb serializes display allocation internally, so concurrent
	// launches never collide on one virtual display.
	xvfb, display, err := startManagedXvfb(xvfbResolution())
	if err != nil {
		return nil, err
	}

	// A managed Xvfb has no GPU, so force software rendering (matches the sibling
	// launchers). stealthEnabled() keeps parity with the other browser launches.
	args := buildChromeArgs(cobrowseLaunchOptions{
		debugPort:  debugPort,
		profileDir: profileDir,
		startURL:   startURL,
		stealth:    stealthEnabled(),
		userAgent:  os.Getenv(EnvCobrowseUserAgent),
		softwareGL: true,
	})

	cmd := exec.Command(chrome, args...)
	cmd.Env = withDisplay(os.Environ(), display)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		if xvfb.Process != nil {
			_ = xvfb.Process.Kill()
		}
		return nil, fmt.Errorf("launch browser: %w", err)
	}

	// Reap each child so a crash is observable (exited) and no zombie is left. stop
	// signals the kill and waits on these channels; it never calls Wait directly.
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	xvfbExited := make(chan struct{})
	go func() {
		_ = xvfb.Wait()
		close(xvfbExited)
	}()

	proc := &cobrowseProc{
		debugPort:  debugPort,
		display:    display,
		browserPID: pidOf(cmd),
		xvfbPID:    pidOf(xvfb),
		exited:     exited,
		stop: func() error {
			return stopBrowserProc(cmd, exited, xvfb, xvfbExited)
		},
	}

	if err := waitForCDPReady(debugPort, 20*time.Second); err != nil {
		// Best-effort teardown so a browser that launched but never exposed CDP does
		// not leak a process or display.
		_ = proc.stop()
		return nil, fmt.Errorf("browser launched but CDP not ready: %w", err)
	}
	return proc, nil
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
func writeSessionPidfile(profileDir string, ownerPID, browserPID, xvfbPID int) {
	path := filepath.Join(profileDir, sessionPidfileName)
	content := fmt.Sprintf("%d\n%d\n%d\n", ownerPID, browserPID, xvfbPID)
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
			return os.MkdirAll(dir, 0o700)
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
	if err := ensureCobrowseBaseDir(m.baseDir); err != nil {
		log.Printf("[cobrowse] refusing session sweep of %s: %v", m.baseDir, err)
		return
	}
	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		return // no parent dir yet: nothing to reap
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(m.baseDir, e.Name())
		ownerPID, browserPID, xvfbPID := readSessionPidfile(dir)
		// Skip a dir whose owning process is still alive: it is not an orphan, and its
		// browser belongs to a live sibling worker.
		if ownerPID > 0 && ownerPID != os.Getpid() && pidAlive(ownerPID) {
			continue
		}
		m.sweepKillIfVerified(browserPID, roleCobrowseBrowser, "browser")
		m.sweepKillIfVerified(xvfbPID, roleCobrowseXvfb, "Xvfb")
		_ = os.RemoveAll(dir)
	}
}

// sweepKillIfVerified kills pid only when processMatchesRole confirms it is still the
// process sweep recorded it as for role. A mismatch or unverifiable PID is logged and
// left alone -- fail closed, never kill on doubt.
func (m *CobrowseSessionManager) sweepKillIfVerified(pid int, role cobrowseProcRole, label string) {
	if pid <= 0 || pid == os.Getpid() {
		return
	}
	if !processMatchesRole(pid, role) {
		log.Printf("[cobrowse] sweep: pid %d recorded as %s no longer matches (or is unverifiable); skipping kill", pid, label)
		return
	}
	_ = pidKiller(pid)
}
