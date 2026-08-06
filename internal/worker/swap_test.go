package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// mockSwapController is a scriptable SwapController for unit tests.
type mockSwapController struct {
	mu sync.Mutex

	resident   map[string]bool // backend -> currently running
	candidates []status.PreemptCandidate
	freeVRAM   uint64
	haveVRAM   bool

	startGate chan struct{} // if non-nil, Start blocks until closed
	startErr  error

	startCount int
	started    []string // backends passed to Start (in order)
	stopped    []string // names passed to StopNonDurable

	readyAfterStart bool // Ready returns true once the backend has been started
}

func newMockController() *mockSwapController {
	return &mockSwapController{
		resident: map[string]bool{},
		haveVRAM: true,
		freeVRAM: 1 << 40, // 1TiB free by default => fits without eviction
	}
}

func (m *mockSwapController) Resident(_ context.Context, backend string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resident[backend]
}

func (m *mockSwapController) PreemptInputs(_ context.Context, exclude string) ([]status.PreemptCandidate, uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]status.PreemptCandidate, 0, len(m.candidates))
	for _, c := range m.candidates {
		if c.Name != exclude {
			out = append(out, c)
		}
	}
	return out, m.freeVRAM, m.haveVRAM
}

func (m *mockSwapController) StopNonDurable(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = append(m.stopped, name)
	// Freeing a candidate's VRAM: remove it and credit its bytes.
	for i, c := range m.candidates {
		if c.Name == name {
			m.freeVRAM += c.VRAMBytes
			m.candidates = append(m.candidates[:i], m.candidates[i+1:]...)
			break
		}
	}
	return nil
}

func (m *mockSwapController) Start(_ context.Context, backend, _ string) error {
	// Count the ATTEMPT before any gate, so an in-flight (blocked) swap is still
	// observable as a started swap.
	m.mu.Lock()
	m.startCount++
	m.started = append(m.started, backend)
	gate := m.startGate
	startErr := m.startErr
	m.mu.Unlock()

	if gate != nil {
		<-gate
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if startErr != nil {
		return startErr
	}
	if m.readyAfterStart {
		m.resident[backend] = true
	}
	return nil
}

func (m *mockSwapController) Ready(_ context.Context, backend string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resident[backend]
}

// newTestManager builds a manager with fast, deterministic timing.
func newTestManager(ctrl SwapController) *SwapManager {
	m := NewSwapManager(ctrl)
	m.waitBudget = 40 * time.Millisecond
	m.minResidency = time.Minute
	m.backgroundMax = 2 * time.Second
	m.readyPoll = 2 * time.Millisecond
	return m
}

func (m *mockSwapController) startCountVal() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startCount
}

func (m *mockSwapController) stoppedNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.stopped...)
}

// --- tests ---

func TestEnsureResident_AlreadyResident_ServesImmediately(t *testing.T) {
	ctrl := newMockController()
	ctrl.resident["bonsai"] = true
	m := newTestManager(ctrl)

	out, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Ready {
		t.Fatalf("expected Ready=true when engine already resident")
	}
	if ctrl.startCountVal() != 0 {
		t.Fatalf("expected no Start call for a resident engine, got %d", ctrl.startCountVal())
	}
}

func TestEnsureResident_SwapCompletesInBudget_Serves(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = true // becomes ready as soon as Start runs
	m := newTestManager(ctrl)
	m.waitBudget = 2 * time.Second // generous so the fast swap finishes in budget

	out, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Ready {
		t.Fatalf("expected Ready=true after a fast swap")
	}
}

func TestEnsureResident_SwapTooSlow_ReturnsWarming(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = false // never becomes ready
	m := newTestManager(ctrl)

	out, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Ready {
		t.Fatalf("expected Ready=false (warming) when swap exceeds budget")
	}
	if out.ETASeconds < warmingRetryAfter {
		t.Fatalf("expected ETA >= retry_after floor, got %d", out.ETASeconds)
	}
}

func TestEnsureResident_DifferentModelInFlight_WarmsAndStartsNoSecondSwap(t *testing.T) {
	ctrl := newMockController()
	ctrl.startGate = make(chan struct{}) // hold the first swap open
	m := newTestManager(ctrl)
	m.backgroundMax = 5 * time.Second

	// First miss: starts a swap that blocks in Start, so inflight stays set.
	out1, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out1.Ready {
		t.Fatalf("expected first miss to warm while swap is in flight")
	}

	// Second miss for a DIFFERENT model while the first is in flight.
	out2, err := m.EnsureResident(context.Background(), "unlimited-ocr", "baidu/Unlimited-OCR")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out2.Ready {
		t.Fatalf("expected second (different-model) miss to warm, not serve")
	}
	if got := ctrl.startCountVal(); got != 1 {
		t.Fatalf("expected exactly ONE swap started (no thrash), got %d", got)
	}

	close(ctrl.startGate) // let the first swap proceed and exit
}

func TestSwap_MinResidencyFloor_DoesNotEvictRecentlyReady(t *testing.T) {
	ctrl := newMockController()
	ctrl.haveVRAM = true
	ctrl.freeVRAM = 0 // full GPU: a swap MUST evict to fit
	// A candidate large enough that evicting it WOULD free enough for bonsai's
	// provisioning budget (~21.5GB) — so the only thing blocking the swap is the
	// min-residency floor, not a genuine VRAM shortfall.
	ctrl.candidates = []status.PreemptCandidate{
		{Name: "unlimited-ocr", VRAMBytes: 23 << 30, Idle: true},
	}
	m := newTestManager(ctrl)
	// unlimited-ocr became ready 5s ago; the 60s floor protects it.
	m.readyAt["unlimited-ocr"] = m.now().Add(-5 * time.Second)

	out, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	if err != nil {
		t.Fatalf("expected no hard error (transient min-residency block), got %v", err)
	}
	if out.Ready {
		t.Fatalf("expected warming while the only candidate is within min-residency")
	}
	// Give the background op a moment to finish its (no-op) plan.
	time.Sleep(50 * time.Millisecond)
	if names := ctrl.stoppedNames(); len(names) != 0 {
		t.Fatalf("expected NO eviction within min-residency, got %v", names)
	}
}

func TestSwap_CannotFitPinned_HardError(t *testing.T) {
	ctrl := newMockController()
	ctrl.haveVRAM = true
	ctrl.freeVRAM = 0
	ctrl.candidates = []status.PreemptCandidate{
		{Name: "unlimited-ocr", VRAMBytes: 12 << 30, Idle: true, Pinned: true},
	}
	m := newTestManager(ctrl)
	m.waitBudget = 2 * time.Second // let the swap finish so the error surfaces

	_, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	if err == nil {
		t.Fatalf("expected a hard error when the only VRAM holder is pinned")
	}
}

func TestEnsureResident_BackgroundSwapSurvivesJobCancellation(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = true
	ctrl.startGate = make(chan struct{}) // hold Start until we cancel the job ctx
	m := newTestManager(ctrl)
	m.backgroundMax = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel the job context shortly after the call begins observing.
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	out, err := m.EnsureResident(ctx, "bonsai", "Bonsai-27B")
	if err != nil {
		t.Fatalf("cancellation must not be a hard error, got %v", err)
	}
	if out.Ready {
		t.Fatalf("expected warming when the job ctx is cancelled mid-swap")
	}

	// The background swap keeps running: let Start proceed, then it should finish.
	close(ctrl.startGate)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ctrl.Resident(context.Background(), "bonsai") {
			return // background swap completed despite the cancelled job ctx
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("background swap did not complete after job-context cancellation")
}

// TestSwap_EngineStartedAt_RecordsAttemptAndClearsOnFailure is the supervisor
// half of citadel-cli#705. The readiness gate reads this record to tell a cold
// start apart from an engine that is not running, so it must exist while a start
// is in flight and must NOT survive a start that failed.
func TestSwap_EngineStartedAt_RecordsAttemptAndClearsOnFailure(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = true
	m := newTestManager(ctrl)

	if _, known := m.EngineStartedAt("bonsai"); known {
		t.Fatal("no start should be on record before one is issued")
	}

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("EnsureResident: %v", err)
	}
	waitFor(t, func() bool {
		_, known := m.EngineStartedAt("bonsai")
		return known
	}, "a start attempt must be recorded")

	// A start that fails must not linger as evidence the engine is booting.
	ctrl2 := newMockController()
	ctrl2.startErr = errors.New("compose up failed")
	m2 := newTestManager(ctrl2)
	m2.waitBudget = 2 * time.Second // let the swap finish so the error surfaces
	if _, err := m2.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err == nil {
		t.Fatal("expected a hard error from a failed start")
	}
	if _, known := m2.EngineStartedAt("bonsai"); known {
		t.Error("a failed start must not stay on record")
	}
}

// TestSwap_EngineStartedAt_ClearedOnEviction asserts an engine we just stopped
// is no longer treated as booting.
func TestSwap_EngineStartedAt_ClearedOnEviction(t *testing.T) {
	ctrl := newMockController()
	m := newTestManager(ctrl)
	m.markStartAttempted("unlimited-ocr")
	if _, known := m.EngineStartedAt("unlimited-ocr"); !known {
		t.Fatal("start should be on record")
	}
	m.forget("unlimited-ocr")
	if _, known := m.EngineStartedAt("unlimited-ocr"); known {
		t.Error("an evicted engine must not stay on record as started")
	}
}

// waitFor polls cond until it holds or the timeout elapses.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}
