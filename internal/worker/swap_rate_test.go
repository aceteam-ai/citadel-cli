package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// Tests for citadel-cli#687: the served-once eviction invariant, the per-engine
// residency ceiling derived from the MEASURED load time, and the swap rate bound.

// fullGPUWith builds a manager whose GPU is full and whose only preemption
// candidate is `name` holding enough VRAM that evicting it WOULD fit a bonsai
// swap-in. So whether the eviction happens isolates the residency/rate decision
// from any VRAM arithmetic.
func fullGPUWith(t *testing.T, name string) (*mockSwapController, *SwapManager) {
	t.Helper()
	ctrl := newMockController()
	ctrl.haveVRAM = true
	ctrl.freeVRAM = 0
	ctrl.candidates = []status.PreemptCandidate{
		{Name: name, VRAMBytes: 23 << 30, Idle: true},
	}
	// The swapped-in engine becomes ready as soon as it starts, so a swap that is
	// allowed to proceed finishes immediately instead of polling out the
	// background ceiling. Keeps these tests off the clock: `release.sh` gates on
	// `go test ./...`, so seconds spent sleeping here are seconds on every release.
	ctrl.readyAfterStart = true
	return ctrl, newTestManager(ctrl)
}

// fillEvictingSwaps seeds the ledger with n completed swaps that each evicted an
// engine, as if the node had already been swapping for a while.
func fillEvictingSwaps(m *SwapManager, n int, age time.Duration) {
	for i := 0; i < n; i++ {
		m.swaps = append(m.swaps, SwapRecord{
			Backend:   "filler",
			Evicted:   []string{"someone"},
			StartedAt: m.now().Add(-age),
			Outcome:   swapOutcomeReady,
		})
	}
}

// expectNoEviction waits out the background swap and asserts nothing was stopped.
func expectNoEviction(t *testing.T, ctrl *mockSwapController) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if names := ctrl.stoppedNames(); len(names) != 0 {
		t.Fatalf("expected NO eviction, got %v", names)
	}
}

// TestSwap_UnservedEngineIsProtectedUntilItServes is the core #687 invariant. A
// vLLM that loaded for ~90s and became ready 70s ago is PAST the 60s
// min-residency floor, but has served nothing — evicting it now would make the
// whole load pure waste, which is the reported failure (a 78s load under a 60s
// floor).
func TestSwap_UnservedEngineIsProtectedUntilItServes(t *testing.T) {
	ctrl, m := fullGPUWith(t, "vllm")
	// Ready 70s ago: past the 60s floor, inside vllm's 90s load-derived ceiling.
	m.readyAt["vllm"] = m.now().Add(-70 * time.Second)

	out, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	if err != nil {
		t.Fatalf("expected a transient block, not a hard error: %v", err)
	}
	if out.Ready {
		t.Fatalf("expected warming while the only candidate has served nothing since loading")
	}
	expectNoEviction(t, ctrl)
}

// TestSwap_ServedEngineBecomesEvictable is the other half of the invariant: once
// a request has actually been dispatched to the engine, the load paid for itself
// and normal eviction ordering resumes. Without this the protection would be a
// permanent pin, not a floor.
func TestSwap_ServedEngineBecomesEvictable(t *testing.T) {
	ctrl, m := fullGPUWith(t, "vllm")
	ready := m.now().Add(-70 * time.Second)
	m.readyAt["vllm"] = ready
	m.servedAt["vllm"] = ready.Add(time.Second) // a request arrived after it loaded

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitFor(t, func() bool {
		for _, n := range ctrl.stoppedNames() {
			if n == "vllm" {
				return true
			}
		}
		return false
	}, "an engine that has served a request must be evictable again")
}

// TestSwap_StaleServedStampDoesNotUnprotect guards the comparison direction. The
// served stamp must be compared against THIS residency's readyAt: a request
// served before the engine was last restarted says nothing about the current
// load, and treating it as "has served" would reopen the exact hole #687 closes.
func TestSwap_StaleServedStampDoesNotUnprotect(t *testing.T) {
	ctrl, m := fullGPUWith(t, "vllm")
	ready := m.now().Add(-70 * time.Second)
	m.readyAt["vllm"] = ready
	m.servedAt["vllm"] = ready.Add(-30 * time.Minute) // from a PREVIOUS residency

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectNoEviction(t, ctrl)
}

// TestSwap_UnknownEngineStaysEvictable is the scoping guard. An engine this node
// never swapped in — operator-started, or resident since boot, or from before a
// worker restart — has no readyAt record, and "no record" must NOT read as
// "recently loaded". Otherwise a worker restart would make every long-resident
// engine unevictable, which is worse than the bug being fixed.
func TestSwap_UnknownEngineStaysEvictable(t *testing.T) {
	ctrl, m := fullGPUWith(t, "vllm")
	// Deliberately no readyAt and no servedAt entry for vllm.

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitFor(t, func() bool {
		return len(ctrl.stoppedNames()) > 0
	}, "an engine with no residency record must stay evictable")
}

// TestSwap_MeasuredLoadRaisesResidencyCeiling is the "per engine, measured"
// requirement. ollama's table estimate (60s) equals the min-residency floor, so
// the estimate alone protects nothing at 70s. Once this node has MEASURED a 120s
// load for it, the same engine at the same age is protected — which is the whole
// point of measuring rather than trusting the table.
func TestSwap_MeasuredLoadRaisesResidencyCeiling(t *testing.T) {
	// Without a measurement: the table estimate does not stretch past the floor.
	ctrl, m := fullGPUWith(t, "ollama")
	m.readyAt["ollama"] = m.now().Add(-70 * time.Second)
	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitFor(t, func() bool { return len(ctrl.stoppedNames()) > 0 },
		"with only a 60s estimate, a 70s-old unserved engine is past its ceiling")

	// With a measured 120s load, the identical engine at the identical age is
	// protected: the measurement, not the table, decides.
	ctrl2, m2 := fullGPUWith(t, "ollama")
	m2.readyAt["ollama"] = m2.now().Add(-70 * time.Second)
	m2.loadMeasured["ollama"] = 120 * time.Second
	if _, err := m2.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectNoEviction(t, ctrl2)
}

// TestSwap_LoadDurationIsMeasuredOnReady asserts the node actually records how
// long a swap took, since every per-engine ceiling above depends on it.
func TestSwap_LoadDurationIsMeasuredOnReady(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = true
	m := newTestManager(ctrl)
	m.waitBudget = 2 * time.Second

	if _, known := m.MeasuredLoad("bonsai"); known {
		t.Fatal("no load should be measured before a swap runs")
	}
	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, known := m.MeasuredLoad("bonsai"); !known {
		t.Fatal("a completed swap must record the engine's measured load time")
	}
}

// TestSwap_ForgetKeepsMeasuredLoad pins the asymmetry in forget(): the residency
// stamps belong to a residency that has ended, but the load measurement belongs
// to the ENGINE and must survive eviction. Dropping it would send the next
// swap-in of this engine back to the coarse estimate — the same defect #688
// describes for lastUsed.
func TestSwap_ForgetKeepsMeasuredLoad(t *testing.T) {
	ctrl := newMockController()
	m := newTestManager(ctrl)
	m.readyAt["vllm"] = m.now()
	m.servedAt["vllm"] = m.now()
	m.loadMeasured["vllm"] = 78 * time.Second

	m.forget("vllm")

	if _, ok := m.readyAt["vllm"]; ok {
		t.Error("readyAt must be cleared on eviction")
	}
	if _, ok := m.servedAt["vllm"]; ok {
		t.Error("servedAt must be cleared on eviction: the residency it described has ended")
	}
	if d, ok := m.MeasuredLoad("vllm"); !ok || d != 78*time.Second {
		t.Errorf("measured load must survive eviction, got %v ok=%v", d, ok)
	}
}

// TestSwap_RateLimit_RefusesEvictingSwapAtCeiling is the swap rate bound. Once
// the node has spent its allowance, another eviction is REFUSED rather than
// performed — and refused before anything is stopped, so the refusal costs the
// box nothing.
func TestSwap_RateLimit_RefusesEvictingSwapAtCeiling(t *testing.T) {
	ctrl, m := fullGPUWith(t, "vllm")
	m.waitBudget = 2 * time.Second // observe the swap to completion so the error surfaces
	fillEvictingSwaps(m, m.maxEvictingPerWindow, 5*time.Minute)

	_, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	var rateErr *SwapRateLimitedError
	if !errors.As(err, &rateErr) {
		t.Fatalf("expected a SwapRateLimitedError at the ceiling, got %v", err)
	}
	if rateErr.Max != m.maxEvictingPerWindow {
		t.Errorf("error should carry the ceiling in force, got %d", rateErr.Max)
	}
	if names := ctrl.stoppedNames(); len(names) != 0 {
		t.Fatalf("a refused swap must evict nothing, got %v", names)
	}
	if ctrl.startCountVal() != 0 {
		t.Fatalf("a refused swap must not start the target engine, got %d starts", ctrl.startCountVal())
	}
}

// TestSwap_RateLimit_AllowsNonEvictingSwap is why the bound counts EVICTIONS and
// not swaps. A node with free VRAM takes nothing away when it starts an engine,
// so its own headroom must never be rate-limited by swaps it made earlier.
func TestSwap_RateLimit_AllowsNonEvictingSwap(t *testing.T) {
	ctrl := newMockController() // default: 1TiB free, so no eviction is planned
	ctrl.readyAfterStart = true
	m := newTestManager(ctrl)
	m.waitBudget = 2 * time.Second
	fillEvictingSwaps(m, m.maxEvictingPerWindow*2, 5*time.Minute) // way over the ceiling

	out, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	if err != nil {
		t.Fatalf("a swap that evicts nothing must not be rate-limited: %v", err)
	}
	if !out.Ready {
		t.Fatal("expected the non-evicting swap to complete")
	}
}

// TestSwap_RateLimit_IgnoresSwapsOutsideTheWindow asserts the bound is a RATE,
// not a lifetime cap: a node that swapped heavily yesterday starts today with a
// full allowance.
func TestSwap_RateLimit_IgnoresSwapsOutsideTheWindow(t *testing.T) {
	ctrl, m := fullGPUWith(t, "vllm")
	m.waitBudget = 2 * time.Second
	fillEvictingSwaps(m, m.maxEvictingPerWindow*2, m.rateWindow+time.Hour) // all stale

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("stale swaps must not count against the window: %v", err)
	}
	waitFor(t, func() bool { return len(ctrl.stoppedNames()) > 0 },
		"a node whose swaps are all outside the window must be allowed to evict")
}

// TestSwap_LedgerRecordsTheSwap covers the counter and per-swap record #687 asks
// the node to emit: before this, an alternating workload could burn the box with
// nothing anywhere counting it.
func TestSwap_LedgerRecordsTheSwap(t *testing.T) {
	_, m := fullGPUWith(t, "vllm")
	m.waitBudget = 2 * time.Second

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitFor(t, func() bool { return m.SwapStats().SwapsPerHour == 1 }, "the swap must be recorded")

	stats := m.SwapStats()
	if stats.EvictingSwapsPerHour != 1 {
		t.Errorf("a swap that stopped an engine must count as evicting, got %d", stats.EvictingSwapsPerHour)
	}
	if stats.MaxEvictingPerHour != m.maxEvictingPerWindow {
		t.Errorf("stats must report the ceiling in force, got %d", stats.MaxEvictingPerHour)
	}
	if len(stats.Recent) != 1 {
		t.Fatalf("expected one record, got %d", len(stats.Recent))
	}
	rec := stats.Recent[0]
	if rec.Backend != "bonsai" || rec.Model != "Bonsai-27B" {
		t.Errorf("record must name the swap, got %+v", rec)
	}
	if rec.Outcome != swapOutcomeReady {
		t.Errorf("expected outcome %q, got %q", swapOutcomeReady, rec.Outcome)
	}
	if len(rec.Evicted) != 1 || rec.Evicted[0] != "vllm" {
		t.Errorf("record must name what was evicted, got %v", rec.Evicted)
	}
}

// TestSwap_LedgerRecordsRateLimitedRefusal asserts a refusal is recorded as a
// refusal. A refused swap that logged as "blocked" or vanished entirely would
// leave an operator asking why the node stopped serving with no answer.
func TestSwap_LedgerRecordsRateLimitedRefusal(t *testing.T) {
	_, m := fullGPUWith(t, "vllm")
	m.waitBudget = 2 * time.Second
	fillEvictingSwaps(m, m.maxEvictingPerWindow, time.Minute)

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err == nil {
		t.Fatal("expected the swap to be refused")
	}
	stats := m.SwapStats()
	last := stats.Recent[len(stats.Recent)-1]
	if last.Outcome != swapOutcomeRateLimited {
		t.Errorf("expected outcome %q, got %q", swapOutcomeRateLimited, last.Outcome)
	}
	if last.Evicting() {
		t.Error("a refused swap evicted nothing and must not count against the next window")
	}
}

// TestSwap_LedgerIsBounded asserts the in-process ledger cannot grow without
// limit on a long-lived worker.
func TestSwap_LedgerIsBounded(t *testing.T) {
	ctrl := newMockController()
	m := newTestManager(ctrl)
	for i := 0; i < swapRecordsKept*3; i++ {
		m.recordSwap(SwapRecord{Backend: "vllm", StartedAt: m.now(), Outcome: swapOutcomeReady})
	}
	if got := len(m.SwapStats().Recent); got > swapRecordsKept {
		t.Errorf("ledger must stay bounded at %d, got %d", swapRecordsKept, got)
	}
}

// TestSwapObserverReceivesEachSwap asserts the record reaches the controller,
// which is how it reaches the node's logs — the "emit" half of #687. A ledger
// nothing reads would leave the operator exactly as blind as before.
func TestSwapObserverReceivesEachSwap(t *testing.T) {
	ctrl := &observingController{mockSwapController: newMockController()}
	ctrl.readyAfterStart = true
	m := newTestManager(ctrl)
	m.waitBudget = 2 * time.Second

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitFor(t, func() bool { return len(ctrl.observedRecords()) == 1 }, "the controller must observe the swap")
	if got := ctrl.observedRecords()[0].Backend; got != "bonsai" {
		t.Errorf("observed record names the wrong engine: %q", got)
	}
	if got := ctrl.observedStats()[0].SwapsPerHour; got != 1 {
		t.Errorf("observed stats must include the swap just recorded, got %d", got)
	}
}

// observingController is a mock controller that also implements swapObserver.
type observingController struct {
	*mockSwapController
	records []SwapRecord
	stats   []SwapStats
}

func (c *observingController) ObserveSwap(rec SwapRecord, stats SwapStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)
	c.stats = append(c.stats, stats)
}

func (c *observingController) observedRecords() []SwapRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]SwapRecord(nil), c.records...)
}

func (c *observingController) observedStats() []SwapStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]SwapStats(nil), c.stats...)
}

// TestHotswap_RateLimited_FailsWithReason pins the wire shape of a refusal.
//
// It must be a FAILURE, not a success carrying a new control status: the
// platform branches on `output.status == "model_warming"` and has no branch for
// anything else, so a success-shaped refusal would be relayed as an empty reply —
// the user asks a question and gets silence, with no error recorded. The machine
// -readable reason rides along for a consumer that later wants to say "this node
// is at its swap limit" specifically.
func TestHotswap_RateLimited_FailsWithReason(t *testing.T) {
	h := NewLLMInferenceHandler().WithSwapper(&fakeSwapper{
		err: &SwapRateLimitedError{Backend: "bonsai", Swaps: 6, Max: 6, Window: time.Hour},
	})

	result, err := h.Execute(context.Background(), hotswapJob(), &MockStreamWriter{})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.Status != JobStatusFailure {
		t.Fatalf("a refusal must be a job FAILURE, got %v", result.Status)
	}
	if got, _ := result.Output["reason"].(string); got != "swap_rate_limited" {
		t.Errorf("reason = %q, want swap_rate_limited", got)
	}
	if got, _ := result.Output["status"].(string); got != "model_unavailable" {
		t.Errorf("status = %q, want model_unavailable", got)
	}
	if got, _ := result.Output["error"].(string); got == "" {
		t.Error("a refusal must carry a human-readable explanation")
	}
}

// TestSwapAccountingDefaults pins the SHIPPED values. The knobs above are vars so
// tests can shrink them without sleeping through an hour; this is what stops a
// test's convenience from silently becoming the node's default.
func TestSwapAccountingDefaults(t *testing.T) {
	if swapRateWindow != time.Hour {
		t.Errorf("swapRateWindow default changed to %v; #687 specifies swaps per hour", swapRateWindow)
	}
	if swapMaxEvictingPerWindow != 6 {
		t.Errorf("swapMaxEvictingPerWindow default changed to %d", swapMaxEvictingPerWindow)
	}
	if swapMinResidency != 60*time.Second {
		t.Errorf("swapMinResidency default changed to %v", swapMinResidency)
	}

	m := NewSwapManager(newMockController())
	if m.rateWindow != swapRateWindow || m.maxEvictingPerWindow != swapMaxEvictingPerWindow {
		t.Error("a manager built for a real node must ship with the default bound in force")
	}
}
