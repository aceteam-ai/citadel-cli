// internal/worker/swap_pull_test.go
//
// Unit tests for citadel-cli#835 (#717 part 2): the tri-state "did this swap
// require a weights pull" signal on SwapRecord.Pulled. cacheDirSize is
// stubbed directly on the manager (mirroring preflight/diskMetrics in
// swap_preflight_test.go) so none of these touch this machine's real
// ~/citadel-cache -- see status.EngineCacheDirSize's doc comment and
// newTestManager's own default stub for why that matters.
package worker

import (
	"context"
	"testing"
	"time"
)

// sizeSequence returns a cacheDirSize stub that yields each element of sizes
// in order (mapped=true throughout), one call at a time -- modeling the
// before/after sampling runSwap performs around Start.
func sizeSequence(sizes ...int64) func(string) (int64, bool) {
	i := 0
	return func(string) (int64, bool) {
		if i >= len(sizes) {
			// Repeat the last value rather than panic if a test's assumptions
			// about call count are ever off -- fail the assertion, not the
			// harness.
			return sizes[len(sizes)-1], true
		}
		v := sizes[i]
		i++
		return v, true
	}
}

func TestSwap_Pulled_TrueWhenCacheDirGrows(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = true
	m := newTestManager(ctrl)
	m.cacheDirSize = sizeSequence(0, 3<<30) // 0 bytes before, 3GiB after

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitFor(t, func() bool { return len(m.SwapStats().Recent) == 1 }, "the swap must be recorded")

	rec := m.SwapStats().Recent[0]
	if rec.Pulled == nil {
		t.Fatal("expected Pulled to be set (non-nil) for a mapped engine")
	}
	if !*rec.Pulled {
		t.Errorf("expected Pulled=true when the cache dir grew, got false")
	}
}

func TestSwap_Pulled_FalseWhenCacheDirUnchanged(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = true
	m := newTestManager(ctrl)
	m.cacheDirSize = sizeSequence(5<<30, 5<<30) // unchanged -- resident-cache start

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitFor(t, func() bool { return len(m.SwapStats().Recent) == 1 }, "the swap must be recorded")

	rec := m.SwapStats().Recent[0]
	if rec.Pulled == nil {
		t.Fatal("expected Pulled to be set (non-nil) for a mapped engine")
	}
	if *rec.Pulled {
		t.Errorf("expected Pulled=false when the cache dir did not grow, got true")
	}
}

func TestSwap_Pulled_NilWhenEngineUnmapped(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = true
	m := newTestManager(ctrl)
	// mapped=false unconditionally: an engine absent from
	// services.EngineCacheDirs (e.g. a future ServiceMap entry not yet added
	// there) must NEVER resolve Pulled to a guessed false -- the #717 rule
	// this field exists to honor.
	m.cacheDirSize = func(string) (int64, bool) { return 0, false }

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitFor(t, func() bool { return len(m.SwapStats().Recent) == 1 }, "the swap must be recorded")

	rec := m.SwapStats().Recent[0]
	if rec.Pulled != nil {
		t.Errorf("expected Pulled=nil (unknown) for an unmapped engine, got %v", *rec.Pulled)
	}
}

// TestSwap_Pulled_NilWhenSwapNeverReachesReady covers a swap that starts but
// never becomes ready within the background ceiling (a "warming" outcome):
// Pulled must stay nil rather than report a premature comparison, since the
// pull -- if any -- may still be in progress when the outcome is recorded.
func TestSwap_Pulled_NilWhenSwapNeverReachesReady(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = false // never becomes ready
	m := newTestManager(ctrl)
	m.backgroundMax = 30 * time.Millisecond
	m.readyPoll = 2 * time.Millisecond
	m.cacheDirSize = sizeSequence(0, 3<<30) // would report "pulled" if compared

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitFor(t, func() bool { return len(m.SwapStats().Recent) == 1 }, "the swap must be recorded")

	rec := m.SwapStats().Recent[0]
	if rec.Outcome != swapOutcomeWarming {
		t.Fatalf("expected outcome %q, got %q", swapOutcomeWarming, rec.Outcome)
	}
	if rec.Pulled != nil {
		t.Errorf("expected Pulled=nil when the swap never reached ready, got %v", *rec.Pulled)
	}
}
