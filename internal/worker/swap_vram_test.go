package worker

import (
	"context"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// Tests for citadel-cli#689: the swap planner's fit check must prefer a
// MEASURED VRAM footprint (with a safety margin, citadel-cli#874 review) over
// the coarse engineVRAMEstimateMB provisioning budget once this node has
// actually observed one, and must fall back to the table when no measurement
// exists yet. No real nvidia-smi/docker is touched — both the footprint and
// the estimate are injected via the mock controller / the manager's own
// accessors. The recording path is asynchronous (citadel-cli#874 review:
// measurement must never delay the readiness signal), so tests that assert a
// measurement WAS recorded poll with waitFor; tests that assert one was NOT
// recorded rely on the mock's MeasuredVRAM being synchronous/instant so a
// short settle delay is enough (see the comment at each such test).

// TestRequiredVRAMBytes_FallsBackToTableWhenUnmeasured is the "non-resident
// engine, no measurement" half of #689: nothing can measure a model that has
// never been loaded, so the fit check must use the table.
func TestRequiredVRAMBytes_FallsBackToTableWhenUnmeasured(t *testing.T) {
	ctrl := newMockController()
	m := newTestManager(ctrl)

	estimate := m.requiredVRAM("bonsai")
	if estimate == 0 {
		t.Fatalf("expected a non-zero table estimate for bonsai")
	}

	if got := m.requiredVRAMBytes("bonsai", "Bonsai-27B"); got != estimate {
		t.Fatalf("requiredVRAMBytes = %d, want the table estimate %d (no measurement recorded)", got, estimate)
	}
}

// TestVRAMFitBytes_AppliesMargin pins the pure margin function: the fit value
// is measured × vramFitMarginFactor, not the raw measurement (citadel-cli#874
// review finding 1).
func TestVRAMFitBytes_AppliesMargin(t *testing.T) {
	measured := uint64(14) << 30 // 14GiB
	got := vramFitBytes(measured)
	want := uint64(float64(measured) * vramFitMarginFactor)
	if got != want {
		t.Fatalf("vramFitBytes(%d) = %d, want %d", measured, got, want)
	}
	if got == measured {
		t.Fatalf("vramFitBytes returned the raw measurement (%d) unmodified; margin was not applied", measured)
	}
	if got <= measured {
		t.Fatalf("vramFitBytes(%d) = %d, want a value strictly greater than the raw measurement (margin must inflate, not shrink)", measured, got)
	}
}

// TestRequiredVRAMBytes_PrefersMeasuredOverTable is the "resident engine with a
// measured footprint" half of #689 — the live scenario the issue reports:
// unlimited-ocr measured ~14GB against a 20GB table budget. Once a measurement
// is on record for a (backend, model) pair, the fit check must use the
// MARGINED measurement (vramFitBytes), not the raw reading and not the padded
// estimate.
func TestRequiredVRAMBytes_PrefersMeasuredOverTable(t *testing.T) {
	ctrl := newMockController()
	m := newTestManager(ctrl)

	estimate := m.requiredVRAM("unlimited-ocr")
	measured := uint64(14) << 30 // ~14GB, well under the 20GB table budget

	m.recordMeasuredVRAM("unlimited-ocr", "baidu/Unlimited-OCR", measured)

	got := m.requiredVRAMBytes("unlimited-ocr", "baidu/Unlimited-OCR")
	want := vramFitBytes(measured)
	if got != want {
		t.Fatalf("requiredVRAMBytes = %d, want the margined measured value %d", got, want)
	}
	if got == measured {
		t.Fatalf("requiredVRAMBytes returned the RAW measured value (%d) with no margin applied", measured)
	}
	if got >= estimate {
		t.Fatalf("requiredVRAMBytes (%d) is not below the table estimate (%d); the margined measurement should still be a real improvement", got, estimate)
	}

	// A DIFFERENT model on the SAME backend must not inherit the measurement —
	// footprints differ by model, so the cache is keyed on the pair, not the
	// backend alone.
	if got := m.requiredVRAMBytes("unlimited-ocr", "some-other-model"); got != estimate {
		t.Fatalf("requiredVRAMBytes for an unmeasured model = %d, want the table estimate %d", got, estimate)
	}
}

// TestSwap_MarginPreventsUnsafeCoResidency is the citadel-cli#874 review's
// second ask: two engines whose RAW measurements would both fit resident at
// once (a zero-margin fit check admits the incoming one without eviction) must
// NOT both end up resident once the margin is applied -- the margined
// requirement must force an eviction the raw number would have skipped.
//
// Setup: unlimited-ocr is already resident, holding 14GiB, and is the ONLY
// preemption candidate. bonsai is swapping in; it was measured at 6GiB on a
// prior residency. Free VRAM is exactly 6GiB -- enough for bonsai's RAW
// measurement (would fit with zero eviction), but not enough for bonsai's
// MARGINED requirement (6GiB × 1.15 > 6GiB), so the margin must force
// unlimited-ocr to be evicted to make room.
func TestSwap_MarginPreventsUnsafeCoResidency(t *testing.T) {
	ctrl := newMockController()
	ctrl.haveVRAM = true
	ctrl.freeVRAM = 6 << 30 // exactly bonsai's raw measurement, no margin headroom
	ctrl.candidates = []status.PreemptCandidate{
		{Name: "unlimited-ocr", VRAMBytes: 14 << 30, Idle: true},
	}
	ctrl.readyAfterStart = true
	m := newTestManager(ctrl)
	m.waitBudget = 2 * time.Second

	// bonsai was measured at 6GiB on a prior residency (pre-seed the cache
	// directly -- this test is about the FIT decision, not the recording path).
	m.recordMeasuredVRAM("bonsai", "Bonsai-27B", 6<<30)

	// Sanity: the raw (zero-margin) requirement would have fit without
	// eviction. If this fails, the test setup itself is wrong.
	if raw := uint64(6 << 30); ctrl.freeVRAM < raw {
		t.Fatalf("test setup invalid: free VRAM %d must be >= raw measurement %d", ctrl.freeVRAM, raw)
	}
	// Sanity: the margined requirement must exceed free VRAM, or this test
	// isn't exercising the margin at all.
	if margined := m.requiredVRAMBytes("bonsai", "Bonsai-27B"); margined <= ctrl.freeVRAM {
		t.Fatalf("test setup invalid: margined requirement %d must exceed free VRAM %d", margined, ctrl.freeVRAM)
	}

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitFor(t, func() bool {
		return len(ctrl.stoppedNames()) > 0
	}, "expected the margin to force eviction of unlimited-ocr (raw sum would have fit both resident)")

	names := ctrl.stoppedNames()
	if len(names) != 1 || names[0] != "unlimited-ocr" {
		t.Fatalf("expected unlimited-ocr to be evicted, got %v", names)
	}
}

// TestSwap_RecordsMeasuredVRAMOnceReady exercises the real recording path: a
// full EnsureResident swap-in, after which the controller's injected footprint
// must eventually be on record via MeasuredVRAMBytes. Recording is
// fire-and-forget (citadel-cli#874 review finding 2: it must not delay the
// ready signal), so this polls rather than asserting immediately after
// EnsureResident returns.
func TestSwap_RecordsMeasuredVRAMOnceReady(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = true
	ctrl.measuredVRAM = 6 << 30 // 6GiB actual footprint, well under bonsai's table budget
	m := newTestManager(ctrl)
	m.waitBudget = 2 * time.Second

	out, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Ready {
		t.Fatalf("expected Ready=true after a fast swap")
	}

	var got uint64
	waitFor(t, func() bool {
		var ok bool
		got, ok = m.MeasuredVRAMBytes("bonsai", "Bonsai-27B")
		return ok
	}, "expected a measured VRAM footprint to eventually be recorded after a successful swap")

	if got != ctrl.measuredVRAM {
		t.Fatalf("MeasuredVRAMBytes = %d, want %d", got, ctrl.measuredVRAM)
	}

	// The next swap-in of the SAME (backend, model) must now size off the
	// margined measurement, not the table.
	want := vramFitBytes(ctrl.measuredVRAM)
	if got := m.requiredVRAMBytes("bonsai", "Bonsai-27B"); got != want {
		t.Fatalf("requiredVRAMBytes after a successful swap = %d, want the margined %d", got, want)
	}
}

// TestSwap_NoMeasurementAvailable_NoCacheEntry asserts a controller reporting
// ok=false (no GPU / no footprint signal) never gets cached as a measured
// zero, which would wrongly tell a later fit check "this needs nothing". The
// mock's MeasuredVRAM is synchronous/instant with no simulated I/O latency, so
// the background goroutine settles well within the poll window below; polling
// for a fixed short window (rather than a single immediate check) tolerates
// scheduler jitter without weakening the assertion.
func TestSwap_NoMeasurementAvailable_NoCacheEntry(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = true
	// ctrl.measuredVRAM left at its zero value => MeasuredVRAM reports ok=false.
	m := newTestManager(ctrl)
	m.waitBudget = 2 * time.Second

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, ok := m.MeasuredVRAMBytes("bonsai", "Bonsai-27B"); ok {
			t.Fatalf("expected no measurement ever cached when the controller reports none available")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSwap_OllamaNeverCachesMeasuredVRAM pins the guard that closes the
// ollama gap: Ready()==true for ollama means a model is merely LISTED, not
// resident (SwapController.Ready's doc comment) -- it lazy-loads on first
// request. A swap-in must not cache whatever cold-VRAM reading the controller
// happens to return at that moment, since vramMeasured deliberately survives
// eviction and a bad reading would stay wrong for the process lifetime. No
// goroutine is ever launched for ollama (vramMeasurableOnReady gates it
// BEFORE the `go` call), so this assertion is deterministic immediately after
// EnsureResident returns -- there is nothing async to race against.
func TestSwap_OllamaNeverCachesMeasuredVRAM(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = true
	ctrl.measuredVRAM = 300 << 20 // a plausible cold/near-idle reading, not a real load
	m := newTestManager(ctrl)
	m.waitBudget = 2 * time.Second

	if _, err := m.EnsureResident(context.Background(), "ollama", "llama3.1:8b"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got, ok := m.MeasuredVRAMBytes("ollama", "llama3.1:8b"); ok {
		t.Fatalf("expected no VRAM measurement cached for ollama, got %d", got)
	}

	// The fit check for a later ollama swap-in must still fall back to the
	// table, unaffected by the (never-recorded) reading.
	estimate := m.requiredVRAM("ollama")
	if got := m.requiredVRAMBytes("ollama", "llama3.1:8b"); got != estimate {
		t.Fatalf("requiredVRAMBytes = %d, want the table estimate %d", got, estimate)
	}
}

// TestSwap_ForgetKeepsMeasuredVRAM mirrors TestSwap_ForgetKeepsMeasuredLoad:
// vramMeasured belongs to the (engine, model) pair, not the residency, so an
// eviction must not drop it -- the next swap-in of the same pair would
// otherwise fall back to the padded table estimate for no reason.
func TestSwap_ForgetKeepsMeasuredVRAM(t *testing.T) {
	ctrl := newMockController()
	m := newTestManager(ctrl)
	m.readyAt["vllm"] = m.now()
	m.servedAt["vllm"] = m.now()
	m.recordMeasuredVRAM("vllm", "some-model", 9<<30)

	m.forget("vllm")

	if _, ok := m.readyAt["vllm"]; ok {
		t.Error("readyAt must be cleared on eviction")
	}
	if got, ok := m.MeasuredVRAMBytes("vllm", "some-model"); !ok || got != 9<<30 {
		t.Errorf("measured VRAM must survive eviction, got %d ok=%v", got, ok)
	}
}

// TestRequiredVRAMBytes_ZeroWhenEngineUnknown pins the fail-safe: an engine not
// in the table and never measured reports 0, which preempt() reads as "no
// budget known: skip preemption" rather than fabricating a number.
func TestRequiredVRAMBytes_ZeroWhenEngineUnknown(t *testing.T) {
	ctrl := newMockController()
	m := newTestManager(ctrl)

	if got := m.requiredVRAMBytes("not-a-real-engine", "some-model"); got != 0 {
		t.Fatalf("requiredVRAMBytes for an unknown engine = %d, want 0", got)
	}
}
