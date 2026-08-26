package worker

import (
	"context"
	"testing"
	"time"
)

// Tests for citadel-cli#689: the swap planner's fit check must prefer a
// MEASURED VRAM footprint over the coarse engineVRAMEstimateMB provisioning
// budget once this node has actually observed one, and must fall back to the
// table when no measurement exists yet. No real nvidia-smi/docker is touched —
// both the footprint and the estimate are injected via the mock controller /
// the manager's own accessors.

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

// TestRequiredVRAMBytes_PrefersMeasuredOverTable is the "resident engine with a
// measured footprint" half of #689 — the live scenario the issue reports:
// unlimited-ocr measured ~14GB against a 20GB table budget. Once a measurement
// is on record for a (backend, model) pair, the fit check must use it instead
// of the padded estimate.
func TestRequiredVRAMBytes_PrefersMeasuredOverTable(t *testing.T) {
	ctrl := newMockController()
	m := newTestManager(ctrl)

	estimate := m.requiredVRAM("unlimited-ocr")
	measured := uint64(14) << 30 // ~14GB, well under the 20GB table budget

	m.recordMeasuredVRAM("unlimited-ocr", "baidu/Unlimited-OCR", measured)

	got := m.requiredVRAMBytes("unlimited-ocr", "baidu/Unlimited-OCR")
	if got != measured {
		t.Fatalf("requiredVRAMBytes = %d, want the measured value %d", got, measured)
	}
	if got == estimate {
		t.Fatalf("requiredVRAMBytes returned the table estimate (%d); measured value was ignored", estimate)
	}

	// A DIFFERENT model on the SAME backend must not inherit the measurement —
	// footprints differ by model, so the cache is keyed on the pair, not the
	// backend alone.
	if got := m.requiredVRAMBytes("unlimited-ocr", "some-other-model"); got != estimate {
		t.Fatalf("requiredVRAMBytes for an unmeasured model = %d, want the table estimate %d", got, estimate)
	}
}

// TestSwap_RecordsMeasuredVRAMOnceReady exercises the real recording path: a
// full EnsureResident swap-in, after which the controller's injected footprint
// must be on record via MeasuredVRAMBytes.
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

	got, ok := m.MeasuredVRAMBytes("bonsai", "Bonsai-27B")
	if !ok {
		t.Fatalf("expected a measured VRAM footprint to be recorded after a successful swap")
	}
	if got != ctrl.measuredVRAM {
		t.Fatalf("MeasuredVRAMBytes = %d, want %d", got, ctrl.measuredVRAM)
	}

	// The next swap-in of the SAME (backend, model) must now size off the
	// measurement, not the table.
	if got := m.requiredVRAMBytes("bonsai", "Bonsai-27B"); got != ctrl.measuredVRAM {
		t.Fatalf("requiredVRAMBytes after a successful swap = %d, want the measured %d", got, ctrl.measuredVRAM)
	}
}

// TestSwap_NoMeasurementAvailable_NoCacheEntry asserts a controller reporting
// ok=false (no GPU / no footprint signal) never gets cached as a measured
// zero, which would wrongly tell a later fit check "this needs nothing".
func TestSwap_NoMeasurementAvailable_NoCacheEntry(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = true
	// ctrl.measuredVRAM left at its zero value => MeasuredVRAM reports ok=false.
	m := newTestManager(ctrl)
	m.waitBudget = 2 * time.Second

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := m.MeasuredVRAMBytes("bonsai", "Bonsai-27B"); ok {
		t.Fatalf("expected no measurement cached when the controller reports none available")
	}
}

// TestSwap_OllamaNeverCachesMeasuredVRAM pins the guard that closes the
// ollama gap: Ready()==true for ollama means a model is merely LISTED, not
// resident (SwapController.Ready's doc comment) -- it lazy-loads on first
// request. A swap-in must not cache whatever cold-VRAM reading the controller
// happens to return at that moment, since vramMeasured deliberately survives
// eviction and a bad reading would stay wrong for the process lifetime.
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
