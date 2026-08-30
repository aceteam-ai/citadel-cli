package pairingdisplay

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRenderer is an in-memory Renderer with no /dev access, for hermetic
// tests of Manager's lifecycle logic.
type fakeRenderer struct {
	mu sync.Mutex

	target        string
	resolveOK     bool
	resolveReason string

	showResult RenderResult
	showCalls  []ShowRequest

	clearErr   error
	clearCalls []string // targets Clear was called with
	clearNotes []string
	surfaces   []string
}

func newFakeRenderer() *fakeRenderer {
	return &fakeRenderer{
		target:     "/dev/fake0",
		resolveOK:  true,
		showResult: RenderResult{Delivered: true, Surface: "console"},
	}
}

func (f *fakeRenderer) ResolveTarget() (string, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.resolveOK {
		return "", f.resolveReason, false
	}
	return f.target, "", true
}

func (f *fakeRenderer) Show(target string, req ShowRequest) RenderResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.showCalls = append(f.showCalls, req)
	return f.showResult
}

func (f *fakeRenderer) Clear(target string, note string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearCalls = append(f.clearCalls, target)
	f.clearNotes = append(f.clearNotes, note)
	return f.clearErr
}

func (f *fakeRenderer) DetectSurfaces() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.surfaces
}

func (f *fakeRenderer) clearCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.clearCalls)
}

func (f *fakeRenderer) showCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.showCalls)
}

func TestManager_ShowDelivers(t *testing.T) {
	r := newFakeRenderer()
	m := NewManager(r)

	out := m.Show("12345678", time.Minute, "gr_1", "Agent Ops")
	if !out.Delivered || out.Surface != "console" {
		t.Fatalf("expected delivered console, got %+v", out)
	}
	if r.showCount() != 1 {
		t.Fatalf("expected 1 Show call, got %d", r.showCount())
	}
}

func TestManager_ShowResolveFailure(t *testing.T) {
	r := newFakeRenderer()
	r.resolveOK = false
	r.resolveReason = "no_console"
	m := NewManager(r)

	out := m.Show("12345678", time.Minute, "gr_1", "")
	if out.Delivered {
		t.Fatalf("expected not delivered, got %+v", out)
	}
	if out.Reason != "no_console" {
		t.Fatalf("expected reason no_console, got %q", out.Reason)
	}
	if r.showCount() != 0 {
		t.Fatalf("Show must not be called when resolve fails, got %d calls", r.showCount())
	}
}

func TestManager_ShowRenderFailureLeavesNoMarker(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRenderer()
	r.showResult = RenderResult{Reason: "graphical_session"}
	m := NewManager(r)
	m.stateDir = dir

	out := m.Show("12345678", time.Minute, "gr_1", "")
	if out.Delivered {
		t.Fatalf("expected not delivered, got %+v", out)
	}
	if out.Reason != "graphical_session" {
		t.Fatalf("expected reason graphical_session, got %q", out.Reason)
	}
	if _, err := os.Stat(filepath.Join(dir, stateMarkerFile)); !os.IsNotExist(err) {
		t.Fatalf("expected no marker file after a failed render, stat err=%v", err)
	}
}

func TestManager_ClearEarly(t *testing.T) {
	r := newFakeRenderer()
	m := NewManager(r)
	m.Show("12345678", time.Hour, "gr_1", "")

	out := m.Clear("gr_1")
	if !out.Cleared {
		t.Fatalf("expected cleared, got %+v", out)
	}
	if r.clearCount() != 1 {
		t.Fatalf("expected 1 Clear call, got %d", r.clearCount())
	}

	// Idempotent: a second clear for the same (now-absent) grant is a
	// no-op success, never an error (design doc §8.2).
	out2 := m.Clear("gr_1")
	if out2.Cleared || out2.Reason != "not_displayed" {
		t.Fatalf("expected idempotent not_displayed, got %+v", out2)
	}
}

func TestManager_ClearWrongGrantIsNoOp(t *testing.T) {
	r := newFakeRenderer()
	m := NewManager(r)
	m.Show("12345678", time.Hour, "gr_1", "")

	out := m.Clear("gr_OTHER")
	if out.Cleared {
		t.Fatalf("expected not cleared for mismatched grant, got %+v", out)
	}
	if out.Reason != "not_displayed" {
		t.Fatalf("expected reason not_displayed, got %q", out.Reason)
	}
	if r.clearCount() != 0 {
		t.Fatalf("Clear must not touch the renderer for a mismatched grant, got %d calls", r.clearCount())
	}

	// The original grant is still cleanly clearable afterward.
	out2 := m.Clear("gr_1")
	if !out2.Cleared {
		t.Fatalf("expected the real grant to still clear, got %+v", out2)
	}
}

func TestManager_ReplacementClearsPrevious(t *testing.T) {
	r := newFakeRenderer()
	m := NewManager(r)

	m.Show("11111111", time.Hour, "gr_1", "")
	m.Show("22222222", time.Hour, "gr_2", "")

	// gr_1 was replaced -- clearing it now is a no-op.
	if out := m.Clear("gr_1"); out.Cleared {
		t.Fatalf("expected gr_1 to already be replaced, got %+v", out)
	}
	// gr_2 is the live one.
	if out := m.Clear("gr_2"); !out.Cleared {
		t.Fatalf("expected gr_2 to clear, got %+v", out)
	}
	if r.showCount() != 2 {
		t.Fatalf("expected 2 Show calls, got %d", r.showCount())
	}
}

func TestManager_TTLExpiryAutoClear(t *testing.T) {
	r := newFakeRenderer()
	m := NewManager(r)

	m.Show("12345678", 20*time.Millisecond, "gr_1", "")

	deadline := time.Now().Add(2 * time.Second)
	for r.clearCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r.clearCount() != 1 {
		t.Fatalf("expected TTL expiry to trigger exactly one Clear, got %d", r.clearCount())
	}

	// After expiry, the pending state is gone.
	out := m.Clear("gr_1")
	if out.Cleared {
		t.Fatalf("expected already-expired grant to be a no-op clear, got %+v", out)
	}
}

func TestManager_ExpiryAfterReplacementDoesNotClearNewCode(t *testing.T) {
	r := newFakeRenderer()
	m := NewManager(r)

	m.Show("11111111", 15*time.Millisecond, "gr_1", "")
	// Replace before gr_1's timer fires.
	m.Show("22222222", time.Hour, "gr_2", "")

	time.Sleep(60 * time.Millisecond)

	// gr_2 must still be live -- the stale gr_1 timer must not have cleared it.
	out := m.Clear("gr_2")
	if !out.Cleared {
		t.Fatalf("expected gr_2 to still be displayed after gr_1's stale timer fired, got %+v", out)
	}
}

func TestManager_MarkerWrittenAndRemoved(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRenderer()
	m := NewManager(r)
	m.stateDir = dir

	const sentinel = "SENTINEL-DO-NOT-LEAK-99999999"
	m.Show(sentinel, time.Hour, "gr_1", "someone")

	raw, err := os.ReadFile(filepath.Join(dir, stateMarkerFile))
	if err != nil {
		t.Fatalf("expected marker file to exist: %v", err)
	}
	if strings.Contains(string(raw), sentinel) {
		t.Fatalf("crash marker leaked the code: %s", raw)
	}
	if !strings.Contains(string(raw), "gr_1") {
		t.Fatalf("expected marker to contain the grant_request_id, got %s", raw)
	}

	m.Clear("gr_1")
	if _, err := os.Stat(filepath.Join(dir, stateMarkerFile)); !os.IsNotExist(err) {
		t.Fatalf("expected marker file removed after Clear, stat err=%v", err)
	}
}

func TestManager_ReconcileStaleClearsAndRemovesMarker(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRenderer()
	m := NewManager(r)
	m.stateDir = dir

	// Simulate a marker left by a previous, crashed process -- written
	// directly, bypassing Show, since this Manager instance never rendered
	// anything itself.
	marker := crashMarker{Target: "/dev/fake0", ExpiresAt: time.Now().Add(time.Hour), GrantRequestID: "gr_crashed"}
	if err := m.writeMarkerLocked(marker); err != nil {
		t.Fatalf("writeMarkerLocked: %v", err)
	}

	if found := m.ReconcileStale(); !found {
		t.Fatalf("expected ReconcileStale to find the marker")
	}
	if r.clearCount() != 1 {
		t.Fatalf("expected ReconcileStale to clear the console, got %d calls", r.clearCount())
	}
	if _, err := os.Stat(filepath.Join(dir, stateMarkerFile)); !os.IsNotExist(err) {
		t.Fatalf("expected marker removed after reconcile, stat err=%v", err)
	}

	// Idempotent: nothing left to reconcile the second time.
	if found := m.ReconcileStale(); found {
		t.Fatalf("expected second ReconcileStale to find nothing")
	}
	if r.clearCount() != 1 {
		t.Fatalf("expected no additional Clear call, got %d", r.clearCount())
	}
}

func TestManager_ShutdownClearsPending(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRenderer()
	m := NewManager(r)
	m.stateDir = dir

	m.Show("12345678", time.Hour, "gr_1", "")
	m.Shutdown()

	if r.clearCount() != 1 {
		t.Fatalf("expected Shutdown to clear the display, got %d calls", r.clearCount())
	}
	if _, err := os.Stat(filepath.Join(dir, stateMarkerFile)); !os.IsNotExist(err) {
		t.Fatalf("expected marker removed after Shutdown, stat err=%v", err)
	}

	// Shutdown with nothing pending is a harmless no-op.
	r2 := newFakeRenderer()
	m2 := NewManager(r2)
	m2.Shutdown()
	if r2.clearCount() != 0 {
		t.Fatalf("expected no Clear call when nothing is pending, got %d", r2.clearCount())
	}
}

func TestManager_NilRendererFailsClosed(t *testing.T) {
	m := NewManager(nil)
	out := m.Show("12345678", time.Minute, "gr_1", "")
	if out.Delivered {
		t.Fatalf("expected not delivered with a nil renderer, got %+v", out)
	}
	if out.Reason != "unsupported_os" {
		t.Fatalf("expected reason unsupported_os, got %q", out.Reason)
	}
}

func TestDetectSurfaces_NoPanic(t *testing.T) {
	// Exercises the real platform renderer (whichever build tag is active).
	// Environment-dependent (a CI container usually has no VT subsystem), so
	// this only asserts it does not panic and returns a well-formed slice.
	_ = DetectSurfaces()
}
