package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// Arm the fake clock only after the first real fake-controller Start has failed.
// The next clock call is the terminal record, after an actual recorded eviction.
type swapCompletionBarrierController struct {
	*mockSwapController
	firstStart atomic.Bool
	afterStart func()
}

func (c *swapCompletionBarrierController) Start(ctx context.Context, backend, model string) error {
	err := c.mockSwapController.Start(ctx, backend, model)
	if c.firstStart.CompareAndSwap(false, true) {
		c.afterStart()
	}
	return err
}

func awaitSwapCompletionSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal(label)
	}
}

func TestSwapCompletionAccountsEvictionBeforeNextAdmission(t *testing.T) {
	base := newMockController()
	base.freeVRAM = 0
	base.candidates = []status.PreemptCandidate{{Name: "old-a", VRAMBytes: 23 << 30, Idle: true}}
	base.startErr = errors.New("synthetic start failure")
	var pauseClock atomic.Bool
	ctrl := &swapCompletionBarrierController{mockSwapController: base, afterStart: func() { pauseClock.Store(true) }}
	m := newTestManager(ctrl)
	m.requiredVRAM = func(string) uint64 { return 10 << 30 }
	m.diskMetrics = func() status.SystemMetrics { return status.SystemMetrics{} }
	m.waitBudget = time.Second
	clock := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	atRecord, releaseRecord := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(releaseRecord) })
	m.now = func() time.Time {
		if pauseClock.CompareAndSwap(true, false) {
			close(atRecord)
			<-releaseRecord
		}
		return clock
	}
	fillEvictingSwaps(m, m.maxEvictingPerWindow-1, time.Minute)

	first := m.startOrJoin("bonsai", "first")
	if first == nil {
		t.Fatal("first swap did not acquire the slot")
	}
	awaitSwapCompletionSignal(t, atRecord, "first eviction never reached terminal accounting")
	if got := base.stoppedNames(); len(got) != 1 || got[0] != "old-a" {
		t.Fatalf("barrier did not follow the actual first eviction: %v", got)
	}
	select {
	case <-first.done:
		t.Fatal("completion published before terminal accounting")
	default:
	}
	if got := m.startOrJoin("bonsai", "first"); got != first {
		t.Fatal("same-backend caller did not join the original pending operation")
	}
	if next := m.startOrJoin("vllm", "second"); next != nil {
		t.Fatal("next target acquired the slot before the prior eviction was accounted")
	}

	// Restore a full fake GPU with another eligible engine. Nothing external is
	// started: the next miss would evict this fake engine without the rate cap.
	base.mu.Lock()
	base.freeVRAM = 0
	base.candidates = []status.PreemptCandidate{{Name: "old-b", VRAMBytes: 23 << 30, Idle: true}}
	base.mu.Unlock()
	release.Do(func() { close(releaseRecord) })
	awaitSwapCompletionSignal(t, first.done, "first completion did not publish")
	if first.err == nil {
		t.Fatal("first start failure was lost")
	}
	stats := m.SwapStats()
	if stats.EvictingSwapsPerHour != m.maxEvictingPerWindow {
		t.Fatalf("completion did not publish the ceiling: %+v", stats)
	}
	last := stats.Recent[len(stats.Recent)-1]
	if last.Backend != "bonsai" || last.Model != "first" || last.Outcome != swapOutcomeFailed || !last.Evicting() {
		t.Fatalf("wrong terminal eviction record: %+v", last)
	}
	_, err := m.EnsureResident(context.Background(), "vllm", "second")
	var limited *SwapRateLimitedError
	if !errors.As(err, &limited) || limited.Swaps != m.maxEvictingPerWindow {
		t.Fatalf("next eviction did not observe the completed count: %v", err)
	}
	if base.startCountVal() != 1 || len(base.stoppedNames()) != 1 {
		t.Fatal("rate-refused successor started or evicted another fake engine")
	}
}

type swapCompletionObservation struct {
	record SwapRecord
	stats  SwapStats
	live   SwapStats
	done   bool
}

type blockingSwapCompletionObserver struct {
	*mockSwapController
	manager  *SwapManager
	first    <-chan struct{}
	observed chan swapCompletionObservation
	release  <-chan struct{}
}

func (c *blockingSwapCompletionObserver) ObserveSwap(rec SwapRecord, stats SwapStats) {
	done := false
	select {
	case <-c.first:
		done = true
	default:
	}
	// Re-enter the read API before signalling the test: an observer invoked
	// under m.mu deadlocks here and fails the bounded observation wait.
	c.observed <- swapCompletionObservation{rec, stats, c.manager.SwapStats(), done}
	<-c.release
}

func TestSwapCompletionPublishesBeforeBlockedReentrantObserver(t *testing.T) {
	base := newMockController()
	base.readyAfterStart = true
	base.startGate = make(chan struct{})
	var releaseStart sync.Once
	defer releaseStart.Do(func() { close(base.startGate) })
	releaseObserver := make(chan struct{})
	defer close(releaseObserver)
	ctrl := &blockingSwapCompletionObserver{
		mockSwapController: base,
		observed:           make(chan swapCompletionObservation, 2),
		release:            releaseObserver,
	}
	m := newTestManager(ctrl)
	m.diskMetrics = func() status.SystemMetrics { return status.SystemMetrics{} }
	ctrl.manager = m
	first := m.startOrJoin("bonsai", "first")
	if first == nil {
		t.Fatal("first swap did not acquire the slot")
	}
	ctrl.first = first.done
	// Publish captured identity before the observer can run, including cleanup
	// when an earlier assertion fails.
	releaseStart.Do(func() { close(base.startGate) })
	var observed swapCompletionObservation
	select {
	case observed = <-ctrl.observed:
	case <-time.After(2 * time.Second):
		t.Fatal("observer could not re-enter the ledger API")
	}
	if !observed.done || observed.record.Backend != "bonsai" || observed.record.Outcome != swapOutcomeReady || observed.stats.SwapsPerHour != 1 || observed.live.SwapsPerHour != 1 {
		t.Fatalf("observer ran before completion/accounting: %+v", observed)
	}
	awaitSwapCompletionSignal(t, first.done, "blocked observer delayed completed caller")
	second := m.startOrJoin("vllm", "second")
	if second == nil {
		t.Fatal("blocked observer retained the single-flight slot")
	}
	awaitSwapCompletionSignal(t, second.done, "blocked observer delayed the next completion")
	if stats := m.SwapStats(); stats.SwapsPerHour != 2 || len(stats.Recent) != 2 {
		t.Fatalf("successor accounting is unavailable while observers block: %+v", stats)
	}
}
