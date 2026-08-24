package platform

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// fakeLauncher returns a launchCobrowseProc replacement that never spawns a real
// browser: it hands each call a distinct debug port and display, records the profile
// dir it was given, and exposes whether its stop closure ran. Restores are the
// caller's job. Not safe for t.Parallel (mutates a package var).
type fakeLauncher struct {
	mu       sync.Mutex
	n        int
	profiles []string
	stopped  int
	// exitedNow, when set, returns an already-closed exited channel so the session
	// reads as crashed (SessionExited) immediately.
	exitedNow bool
}

func (f *fakeLauncher) install(t *testing.T) {
	t.Helper()
	prev := launchCobrowseProc
	launchCobrowseProc = func(profileDir, startURL string) (*cobrowseProc, error) {
		f.mu.Lock()
		f.n++
		n := f.n
		f.profiles = append(f.profiles, profileDir)
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

	m := newCobrowseSessionManager(t.TempDir(), 8)

	a, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start session a: %v", err)
	}
	b, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start session b: %v", err)
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
	m := newCobrowseSessionManager(t.TempDir(), 8)

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
	m := newCobrowseSessionManager(t.TempDir(), 8)

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
	m := newCobrowseSessionManager(t.TempDir(), 2)

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
	m := newCobrowseSessionManager(t.TempDir(), 8)

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
	m := newCobrowseSessionManager(t.TempDir(), 8)
	if _, ok := m.SessionStatus("cb-does-not-exist"); ok {
		t.Errorf("unknown session should not be found")
	}
}

// TestCobrowseSession_Attach flips a running session to attached and back (the #794
// hook point), and refuses to attach a non-running session.
func TestCobrowseSession_Attach(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(t.TempDir(), 8)

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

// TestCobrowseSession_SweepReapsOrphans verifies the startup sweep kills the PIDs
// recorded in an orphaned session dir and removes the dir. Uses the injectable
// pidKiller seam so no real process is signalled.
func TestCobrowseSession_SweepReapsOrphans(t *testing.T) {
	base := t.TempDir()
	orphan := filepath.Join(base, "cb-orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	writeSessionPidfile(orphan, 424242, 424243)

	var killed []int
	prevKiller := pidKiller
	pidKiller = func(pid int) error {
		killed = append(killed, pid)
		return nil
	}
	t.Cleanup(func() { pidKiller = prevKiller })

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
	writeSessionPidfile(dir, 111, 222)
	b, x := readSessionPidfile(dir)
	if b != 111 || x != 222 {
		t.Errorf("pidfile round-trip mismatch: got (%d,%d) want (111,222)", b, x)
	}
	// A missing pidfile yields zeros, never a panic.
	b, x = readSessionPidfile(t.TempDir())
	if b != 0 || x != 0 {
		t.Errorf("missing pidfile should read (0,0), got (%d,%d)", b, x)
	}
}
