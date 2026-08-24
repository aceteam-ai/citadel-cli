package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeLauncher returns a launchCobrowseProc replacement that never spawns a real
// browser: it hands each call a distinct debug port and display, records the profile
// dir it was given, and exposes whether its stop closure ran. Restores are the
// caller's job. Not safe for t.Parallel (mutates a package var).
type fakeLauncher struct {
	mu       sync.Mutex
	n        int
	profiles []string
	urls     []string
	stopped  int
	// exitedNow, when set, returns an already-closed exited channel so the session
	// reads as crashed (SessionExited) immediately.
	exitedNow bool
	// gate, when non-nil, blocks each launch until it is received from, so a test can
	// hold a launch mid-flight and race a concurrent Stop against it.
	gate chan struct{}
}

func (f *fakeLauncher) install(t *testing.T) {
	t.Helper()
	prev := launchCobrowseProc
	launchCobrowseProc = func(profileDir, startURL string) (*cobrowseProc, error) {
		if f.gate != nil {
			<-f.gate
		}
		f.mu.Lock()
		f.n++
		n := f.n
		f.profiles = append(f.profiles, profileDir)
		f.urls = append(f.urls, startURL)
		f.mu.Unlock()
		exited := make(chan struct{})
		if f.exitedNow {
			close(exited)
		}
		return &cobrowseProc{
			debugPort:  9000 + n,
			display:    ":" + strconv.Itoa(50+n),
			browserPID: 100000 + n,
			xvfbPID:    200000 + n,
			exited:     exited,
			stop: func() error {
				f.mu.Lock()
				f.stopped++
				f.mu.Unlock()
				return nil
			},
		}, nil
	}
	t.Cleanup(func() { launchCobrowseProc = prev })
}

// TestCobrowseSession_Isolation is the core acceptance test: two concurrent sessions
// must not share state. Each must get its own profile dir, debug port, and display.
// Mutation check: if the manager handed both sessions one shared profile dir (or one
// shared port), the distinctness assertions below fail.
func TestCobrowseSession_Isolation(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)

	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	a, err := m.StartSession("https://a.example")
	if err != nil {
		t.Fatalf("start session a: %v", err)
	}
	b, err := m.StartSession("https://b.example")
	if err != nil {
		t.Fatalf("start session b: %v", err)
	}

	// The start URL must reach the launcher (mutation check for the dropped-url bug):
	// StartSession's argument has to be threaded through to launchCobrowseProc.
	f.mu.Lock()
	gotURLs := append([]string(nil), f.urls...)
	f.mu.Unlock()
	wantURLs := map[string]bool{"https://a.example": true, "https://b.example": true}
	for _, u := range gotURLs {
		delete(wantURLs, u)
	}
	if len(wantURLs) != 0 {
		t.Errorf("start URLs not passed to launcher; missing %v (got %v)", wantURLs, gotURLs)
	}

	if a.ID == b.ID {
		t.Fatalf("sessions share an id: %s", a.ID)
	}
	if a.Profile == b.Profile {
		t.Fatalf("sessions share a profile dir: %s", a.Profile)
	}
	if a.DebugPort == b.DebugPort {
		t.Fatalf("sessions share a debug port: %d", a.DebugPort)
	}
	if a.Display == b.Display {
		t.Fatalf("sessions share a display: %s", a.Display)
	}
	if a.State != SessionRunning || b.State != SessionRunning {
		t.Fatalf("expected both running, got %q and %q", a.State, b.State)
	}
	for _, st := range []CobrowseSessionStatus{a, b} {
		if _, err := os.Stat(st.Profile); err != nil {
			t.Errorf("profile dir missing for %s: %v", st.ID, err)
		}
	}
}

// TestCobrowseSession_StopRemovesProfileAndSlot verifies stop tears down the browser
// and removes the throwaway profile dir, and that a sibling is untouched.
func TestCobrowseSession_StopRemovesProfileAndSlot(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	a, _ := m.StartSession("")
	b, _ := m.StartSession("")

	if err := m.Stop(a.ID); err != nil {
		t.Fatalf("stop a: %v", err)
	}
	if _, err := os.Stat(a.Profile); !os.IsNotExist(err) {
		t.Errorf("stopped session profile dir should be gone, stat err=%v", err)
	}
	if _, ok := m.SessionStatus(a.ID); ok {
		t.Errorf("stopped session should not be listed")
	}
	if _, ok := m.SessionStatus(b.ID); !ok {
		t.Errorf("sibling session should still be listed after stopping a")
	}
	if _, err := os.Stat(b.Profile); err != nil {
		t.Errorf("sibling profile dir should still exist: %v", err)
	}

	// Double-stop is not an error.
	if err := m.Stop(a.ID); err != nil {
		t.Errorf("double stop should be a no-op, got %v", err)
	}
}

// TestCobrowseSession_StopAll tears down every session (the node-shutdown path).
func TestCobrowseSession_StopAll(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	sessions := []CobrowseSessionStatus{}
	for i := 0; i < 3; i++ {
		st, err := m.StartSession("")
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		sessions = append(sessions, st)
	}

	if err := m.StopAll(); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	if got := len(m.List()); got != 0 {
		t.Errorf("expected 0 sessions after StopAll, got %d", got)
	}
	f.mu.Lock()
	stopped := f.stopped
	f.mu.Unlock()
	if stopped != 3 {
		t.Errorf("expected 3 browser teardowns, got %d", stopped)
	}
	for _, st := range sessions {
		if _, err := os.Stat(st.Profile); !os.IsNotExist(err) {
			t.Errorf("profile dir for %s should be gone after StopAll", st.ID)
		}
	}
}

// TestCobrowseSession_Cap enforces the concurrent-session cap so an unbounded
// StartSession cannot exhaust node resources.
func TestCobrowseSession_Cap(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)

	if _, err := m.StartSession(""); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, err := m.StartSession(""); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if _, err := m.StartSession(""); err == nil {
		t.Fatalf("third start should be rejected by the cap")
	}
}

// TestCobrowseSession_CrashReportsExited verifies a browser that died mid-life is
// reported as SessionExited, not a stale running.
func TestCobrowseSession_CrashReportsExited(t *testing.T) {
	f := &fakeLauncher{exitedNow: true}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	st, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	got, ok := m.SessionStatus(st.ID)
	if !ok {
		t.Fatalf("session not found")
	}
	if got.State != SessionExited {
		t.Errorf("expected state %q for a dead browser, got %q", SessionExited, got.State)
	}
}

// TestCobrowseSession_StatusUnknown confirms an unknown id reports not-found rather
// than a fabricated status.
func TestCobrowseSession_StatusUnknown(t *testing.T) {
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	if _, ok := m.SessionStatus("cb-does-not-exist"); ok {
		t.Errorf("unknown session should not be found")
	}
}

// TestCobrowseSession_Attach flips a running session to attached and back (the #794
// hook point), and refuses to attach a non-running session.
func TestCobrowseSession_Attach(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	st, _ := m.StartSession("")
	if !m.MarkAttached(st.ID) {
		t.Fatalf("attach should succeed for a known session")
	}
	got, _ := m.SessionStatus(st.ID)
	if got.State != SessionAttached {
		t.Errorf("expected attached, got %q", got.State)
	}
	if !m.MarkDetached(st.ID) {
		t.Fatalf("detach should succeed")
	}
	got, _ = m.SessionStatus(st.ID)
	if got.State != SessionRunning {
		t.Errorf("expected running after detach, got %q", got.State)
	}
	if m.MarkAttached("cb-nope") {
		t.Errorf("attach of unknown session should return false")
	}
}

// trustedBaseDir returns a fresh temp dir chmod'd to 0700, modeling what
// ensureCobrowseBaseDir itself creates in production (plain os.MkdirAll(dir, 0o700)
// under os.TempDir()). t.TempDir()'s own per-test subdir is NOT a stand-in for this:
// Go's testing package creates it via MkdirAll(dir, 0777), which under a permissive
// umask can leave it group-writable -- exactly the shape validateCobrowseBaseDirPerms
// exists to reject, but not what production's own dir-creation path produces. Tests
// that exercise sweep()'s actual reap/skip behavior (as opposed to the refusal gate
// itself) need a base dir that passes validation, so they use this instead of a bare
// t.TempDir().
func trustedBaseDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatalf("chmod base dir: %v", err)
		}
	}
	return dir
}

// TestCobrowseSession_SweepReapsOrphans verifies the startup sweep kills the PIDs
// recorded in an orphaned session dir and removes the dir. Uses the injectable
// pidKiller seam so no real process is signalled, and stubs the cmdline-identity
// check to match so the kill path (not the identity check itself, covered separately
// below) is what's under test here.
func TestCobrowseSession_SweepReapsOrphans(t *testing.T) {
	base := trustedBaseDir(t)
	orphan := filepath.Join(base, "cb-orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	// Owner PID 424241 is a dead process (unused), so the dir is a real orphan.
	writeSessionPidfile(orphan, 424241, 424242, 424243)

	var killed []int
	prevKiller := pidKiller
	pidKiller = func(pid int) error {
		killed = append(killed, pid)
		return nil
	}
	t.Cleanup(func() { pidKiller = prevKiller })
	installMatchingCmdlineReader(t)

	m := newCobrowseSessionManager(base, 8)
	m.sweep()

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan dir should be removed by sweep, stat err=%v", err)
	}
	wantKilled := map[int]bool{424242: true, 424243: true}
	for _, pid := range killed {
		delete(wantKilled, pid)
	}
	if len(wantKilled) != 0 {
		t.Errorf("sweep did not kill all recorded orphan pids; missed %v (killed %v)", wantKilled, killed)
	}
}

// TestCobrowseSession_PidfileRoundTrip pins the pidfile format the sweep depends on.
func TestCobrowseSession_PidfileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeSessionPidfile(dir, 111, 222, 333)
	o, b, x := readSessionPidfile(dir)
	if o != 111 || b != 222 || x != 333 {
		t.Errorf("pidfile round-trip mismatch: got (%d,%d,%d) want (111,222,333)", o, b, x)
	}
	// A missing pidfile yields zeros, never a panic.
	o, b, x = readSessionPidfile(t.TempDir())
	if o != 0 || b != 0 || x != 0 {
		t.Errorf("missing pidfile should read (0,0,0), got (%d,%d,%d)", o, b, x)
	}
}

// TestCobrowseSession_SweepSkipsLiveOwner verifies the sweep leaves a session dir
// owned by a STILL-ALIVE process alone -- one citadel process must never reap the
// live sessions of a sibling worker sharing the parent dir. Mutation check: without
// the pidAlive owner guard, this test's kill-count assertion fails.
func TestCobrowseSession_SweepSkipsLiveOwner(t *testing.T) {
	base := trustedBaseDir(t)
	live := filepath.Join(base, "cb-live-sibling")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Use the parent process PID as the owner: it is alive for the duration of the
	// test and differs from our own PID, so it models a live SIBLING worker (the
	// sweep deliberately does not skip its own PID, only other live processes).
	writeSessionPidfile(live, os.Getppid(), 515151, 515152)

	var killed []int
	prevKiller := pidKiller
	pidKiller = func(pid int) error {
		killed = append(killed, pid)
		return nil
	}
	t.Cleanup(func() { pidKiller = prevKiller })

	m := newCobrowseSessionManager(base, 8)
	m.sweep()

	if len(killed) != 0 {
		t.Errorf("sweep must not kill a live owner's browser pids, killed %v", killed)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live sibling's dir must survive the sweep: %v", err)
	}
}

// TestCobrowseSession_StopDuringLaunch is the shutdown-mid-start race: a StopAll that
// races an in-flight launch must not orphan the browser. The launcher is gated so the
// launch is held mid-flight; StopAll runs, then the launch is released. StartSession
// must error AND the freshly launched browser must be torn down (no orphan).
func TestCobrowseSession_StopDuringLaunch(t *testing.T) {
	f := &fakeLauncher{gate: make(chan struct{})}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	startErr := make(chan error, 1)
	go func() {
		_, err := m.StartSession("")
		startErr <- err
	}()

	// Wait until the session slot is registered (StartSession inserted it before
	// blocking in the gated launcher), then stop everything mid-launch.
	waitFor(t, func() bool { return len(m.List()) == 1 })
	if err := m.StopAll(); err != nil {
		t.Fatalf("stop all: %v", err)
	}

	// Release the launch so it completes into a stopped slot.
	close(f.gate)
	err := <-startErr
	if err == nil {
		t.Fatalf("StartSession should error when the session is stopped during launch")
	}

	// The browser that finished launching must have been torn down, not orphaned.
	f.mu.Lock()
	stopped := f.stopped
	f.mu.Unlock()
	if stopped != 1 {
		t.Errorf("browser launched into a stopped slot must be torn down; stop calls=%d", stopped)
	}
	if got := len(m.List()); got != 0 {
		t.Errorf("no session should remain after stop-during-launch, got %d", got)
	}
}

// installMatchingCmdlineReader stubs procCmdlineReader to report a cmdline that
// matches BOTH the browser and Xvfb identity markers for any PID, so a sweep test
// exercising something other than the identity check itself (orphan reaping, owner
// skip) doesn't get tripped up by the fail-closed default.
func installMatchingCmdlineReader(t *testing.T) {
	t.Helper()
	prev := procCmdlineReader
	procCmdlineReader = func(pid int) (string, error) {
		return "/usr/bin/chromium --headless\x00Xvfb :99", nil
	}
	t.Cleanup(func() { procCmdlineReader = prev })
}

// TestCobrowseSession_SweepVerifiesIdentityBeforeKill is the core security-fix test
// (issue #793 review): sweep must NOT SIGKILL a recorded PID whose cmdline does not
// match the expected browser/Xvfb binary -- e.g. because the PID was recycled by the
// OS to an unrelated process after a reboot (the pidfile's base dir is plain /tmp,
// which often survives a reboot). It must still clean up the stale session dir.
func TestCobrowseSession_SweepVerifiesIdentityBeforeKill(t *testing.T) {
	base := trustedBaseDir(t)
	orphan := filepath.Join(base, "cb-orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	// Dead owner -> orphan. Recorded browser/xvfb PIDs are arbitrary numbers standing
	// in for "recycled to some unrelated process" (e.g. sshd) since the reboot.
	const recycledBrowserPID = 909001
	const recycledXvfbPID = 909002
	writeSessionPidfile(orphan, 424241, recycledBrowserPID, recycledXvfbPID)

	var killed []int
	prevKiller := pidKiller
	pidKiller = func(pid int) error {
		killed = append(killed, pid)
		return nil
	}
	t.Cleanup(func() { pidKiller = prevKiller })

	// Simulate the recycled PIDs now belonging to an unrelated process: cmdline
	// present and readable, but it names neither a browser nor Xvfb.
	prevReader := procCmdlineReader
	procCmdlineReader = func(pid int) (string, error) {
		if pid == recycledBrowserPID || pid == recycledXvfbPID {
			return "/usr/sbin/sshd -D\x00", nil
		}
		return "", os.ErrNotExist
	}
	t.Cleanup(func() { procCmdlineReader = prevReader })

	m := newCobrowseSessionManager(base, 8)
	m.sweep()

	if len(killed) != 0 {
		t.Errorf("sweep must not kill a PID whose cmdline no longer matches, killed %v", killed)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("stale orphan dir should still be cleaned up even when the kill is skipped, stat err=%v", err)
	}
}

// TestCobrowseSession_SweepSkipsKillWhenCmdlineUnreadable covers the other fail-closed
// branch: the cmdline read errors outright (process gone between the pidfile read and
// the check, or -- on a non-Linux node -- no /proc at all). Unreadable must be treated
// the same as "mismatched": no kill, but the stale dir is still removed.
func TestCobrowseSession_SweepSkipsKillWhenCmdlineUnreadable(t *testing.T) {
	base := trustedBaseDir(t)
	orphan := filepath.Join(base, "cb-orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	writeSessionPidfile(orphan, 424241, 909003, 909004)

	var killed []int
	prevKiller := pidKiller
	pidKiller = func(pid int) error {
		killed = append(killed, pid)
		return nil
	}
	t.Cleanup(func() { pidKiller = prevKiller })

	prevReader := procCmdlineReader
	procCmdlineReader = func(pid int) (string, error) {
		return "", os.ErrNotExist
	}
	t.Cleanup(func() { procCmdlineReader = prevReader })

	m := newCobrowseSessionManager(base, 8)
	m.sweep()

	if len(killed) != 0 {
		t.Errorf("sweep must not kill a PID it cannot verify, killed %v", killed)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("stale orphan dir should still be cleaned up, stat err=%v", err)
	}
}

// TestCobrowseSession_ProcessMatchesRole pins the marker-matching rules processMatch
// esRole depends on, independent of sweep.
func TestCobrowseSession_ProcessMatchesRole(t *testing.T) {
	prev := procCmdlineReader
	t.Cleanup(func() { procCmdlineReader = prev })

	cases := []struct {
		name    string
		cmdline string
		err     error
		role    cobrowseProcRole
		want    bool
	}{
		{"chrome matches browser", "/usr/bin/google-chrome\x00--headless", nil, roleCobrowseBrowser, true},
		{"chromium matches browser", "/usr/lib/chromium/chromium\x00--headless", nil, roleCobrowseBrowser, true},
		{"xvfb matches xvfb", "/usr/bin/Xvfb\x00:51\x00-screen\x000\x001280x800x24", nil, roleCobrowseXvfb, true},
		{"sshd does not match browser", "/usr/sbin/sshd\x00-D", nil, roleCobrowseBrowser, false},
		{"sshd does not match xvfb", "/usr/sbin/sshd\x00-D", nil, roleCobrowseXvfb, false},
		{"unreadable does not match", "", os.ErrNotExist, roleCobrowseBrowser, false},
		{"empty cmdline does not match", "", nil, roleCobrowseXvfb, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			procCmdlineReader = func(pid int) (string, error) { return tc.cmdline, tc.err }
			if got := processMatchesRole(12345, tc.role); got != tc.want {
				t.Errorf("processMatchesRole(%q, role=%v) = %v, want %v", tc.cmdline, tc.role, got, tc.want)
			}
		})
	}
	if processMatchesRole(0, roleCobrowseBrowser) {
		t.Errorf("pid<=0 must never match")
	}
}

// TestCobrowseSession_SweepRefusesUntrustedBaseDir verifies the second gate: a base
// dir that is NOT owned by the current user, or is group/other-writable, must cause
// sweep to refuse entirely (no reads of its pidfiles trusted), rather than silently
// fixing the permissions. This models a local unprivileged user pre-creating the
// shared /tmp parent before citadel ever runs. Unix-only: the check is a no-op on
// Windows (see cobrowse_basedir_windows.go).
func TestCobrowseSession_SweepRefusesUntrustedBaseDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("base dir permission/ownership check is unix-only")
	}
	parent := t.TempDir()
	base := filepath.Join(parent, "shared-base")
	if err := os.MkdirAll(base, 0o777); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	// MkdirAll's mode is subject to umask, so force world-writable explicitly --
	// models a pre-created, untrusted shared dir regardless of the test host's umask.
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatalf("chmod base: %v", err)
	}

	orphan := filepath.Join(base, "cb-orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	writeSessionPidfile(orphan, 424241, 909005, 909006)
	installMatchingCmdlineReader(t) // would match/kill if sweep got this far

	var killed []int
	prevKiller := pidKiller
	pidKiller = func(pid int) error {
		killed = append(killed, pid)
		return nil
	}
	t.Cleanup(func() { pidKiller = prevKiller })

	m := newCobrowseSessionManager(base, 8)
	m.sweep()

	if len(killed) != 0 {
		t.Errorf("sweep must refuse to act on a world-writable base dir, killed %v", killed)
	}
	// The untrusted dir's own contents must be left exactly alone -- sweep never even
	// looks inside it.
	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("sweep must not touch contents of an untrusted base dir: %v", err)
	}
}

// TestCobrowseSession_EnsureBaseDirCreatesTrusted verifies the normal, no-prior-state
// path: an absent base dir is created 0o700 (so a subsequent sweep in the same process
// trusts it), matching what StartSession's first launch relies on implicitly.
func TestCobrowseSession_EnsureBaseDirCreatesTrusted(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "fresh-base")
	if err := ensureCobrowseBaseDir(dir); err != nil {
		t.Fatalf("ensureCobrowseBaseDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat created dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected a directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Errorf("expected mode 0700, got %o", info.Mode().Perm())
	}
	// Idempotent: calling again on the now-existing, well-formed dir must succeed.
	if err := ensureCobrowseBaseDir(dir); err != nil {
		t.Errorf("second ensureCobrowseBaseDir call should pass validation: %v", err)
	}
}

// waitFor polls cond up to ~2s, failing the test if it never becomes true.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// TestCobrowseSession_RealLaunch exercises the REAL defaultLaunchCobrowseProc path
// (the fake-launcher tests never touch it). It launches two concurrent sessions with
// real Chromium + Xvfb, asserts they land on distinct displays and ports, and that
// StopAll leaves no live browser/Xvfb process behind. Opt-in only (like
// TestMeetingBrowser_Launch): CI containers have no working display even when the
// binaries exist, so binary presence alone is not a safe trigger.
func TestCobrowseSession_RealLaunch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-launch test in -short mode")
	}
	if os.Getenv("CITADEL_BROWSER_INTEGRATION") == "" {
		t.Skip("set CITADEL_BROWSER_INTEGRATION=1 to run the browser-session launch integration test")
	}
	if !ChromiumAvailable() || !XvfbAvailable() {
		t.Skip("no Chromium or Xvfb on this host; skipping browser-session launch test")
	}

	m := newCobrowseSessionManager(filepath.Join(t.TempDir(), cobrowseSessionsDirName), 8)

	a, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start a: %v", err)
	}
	b, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start b: %v", err)
	}
	if a.Display == b.Display {
		t.Fatalf("concurrent real sessions share a display: %s", a.Display)
	}
	if a.DebugPort == b.DebugPort {
		t.Fatalf("concurrent real sessions share a CDP port: %d", a.DebugPort)
	}

	// Capture the browser PIDs before teardown so we can confirm they are gone after.
	var pids []int
	for _, st := range []CobrowseSessionStatus{a, b} {
		if _, bpid, _ := readSessionPidfile(st.Profile); bpid > 0 {
			pids = append(pids, bpid)
		}
	}

	if err := m.StopAll(); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	for _, pid := range pids {
		if pidAlive(pid) {
			t.Errorf("browser pid %d still alive after StopAll (orphaned)", pid)
		}
	}
}
