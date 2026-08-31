// internal/worker/swap_preflight_test.go
//
// Unit tests for citadel-cli#956: the node's own on-demand swap path fails
// fast on a genuinely unserveable engine instead of attempting a doomed pull.
// None of these depend on a real docker/podman daemon or real disk state —
// SwapManager.preflight and .diskMetrics are package-private fields stubbed
// directly, mirroring newTestManager's existing pattern of mutating fields
// after NewSwapManager (see swap_test.go).
package worker

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// TestSwap_PreflightBlocked_ReturnsTypedErrorNotStart asserts a blocked
// preflight fails the swap BEFORE any eviction or Start attempt, with a typed
// error the caller can distinguish from a generic failure.
func TestSwap_PreflightBlocked_ReturnsTypedErrorNotStart(t *testing.T) {
	ctrl, m := fullGPUWith(t, "vllm") // a resident engine, so a block would otherwise evict it
	m.waitBudget = 2 * time.Second
	m.preflight = func(backend string, _ status.SystemMetrics) (bool, string) {
		return true, "image_missing"
	}

	_, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	var preflightErr *SwapPreflightBlockedError
	if !errors.As(err, &preflightErr) {
		t.Fatalf("expected a SwapPreflightBlockedError, got %v", err)
	}
	if preflightErr.Backend != "bonsai" {
		t.Errorf("Backend = %q, want bonsai", preflightErr.Backend)
	}
	if preflightErr.Reason != "image_missing" {
		t.Errorf("Reason = %q, want image_missing", preflightErr.Reason)
	}
	if ctrl.startCountVal() != 0 {
		t.Errorf("a preflight-blocked swap must not attempt Start, got %d starts", ctrl.startCountVal())
	}
	if names := ctrl.stoppedNames(); len(names) != 0 {
		t.Errorf("a preflight-blocked swap must not evict anything to make room for a doomed start, got %v", names)
	}
}

// TestSwap_PreflightBlocked_EachReasonSurfaces pins the three reason strings
// EngineServeablePreflight can hand back, end to end through EnsureResident.
func TestSwap_PreflightBlocked_EachReasonSurfaces(t *testing.T) {
	for _, reason := range []string{"image_missing", "weights_missing", "disk_pressure"} {
		t.Run(reason, func(t *testing.T) {
			ctrl := newMockController()
			m := newTestManager(ctrl)
			m.waitBudget = 2 * time.Second
			m.preflight = func(backend string, _ status.SystemMetrics) (bool, string) {
				return true, reason
			}

			_, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
			var preflightErr *SwapPreflightBlockedError
			if !errors.As(err, &preflightErr) {
				t.Fatalf("expected a SwapPreflightBlockedError, got %v", err)
			}
			if preflightErr.Reason != reason {
				t.Errorf("Reason = %q, want %q", preflightErr.Reason, reason)
			}
			if ctrl.startCountVal() != 0 {
				t.Errorf("expected no Start attempt, got %d", ctrl.startCountVal())
			}
		})
	}
}

// TestSwap_PreflightNotBlocked_ProceedsNormally asserts a passing preflight is
// a no-op on the existing swap path: the engine starts and becomes ready
// exactly as it would have before #956.
func TestSwap_PreflightNotBlocked_ProceedsNormally(t *testing.T) {
	ctrl := newMockController()
	ctrl.readyAfterStart = true
	m := newTestManager(ctrl)
	m.waitBudget = 2 * time.Second
	m.preflight = func(backend string, _ status.SystemMetrics) (bool, string) {
		return false, ""
	}

	out, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Ready {
		t.Fatal("expected the swap to complete and report ready")
	}
	if ctrl.startCountVal() != 1 {
		t.Errorf("expected exactly one Start attempt, got %d", ctrl.startCountVal())
	}
}

// TestSwap_PreflightNotCalledOnResidentHit is the "no added latency on the hot
// path" contract: EnsureResident's resident fast path must never reach the
// preflight (or runSwap) at all — a docker-inspect on every inference request
// to an already-resident engine is exactly the cost #956 must not add.
func TestSwap_PreflightNotCalledOnResidentHit(t *testing.T) {
	ctrl := newMockController()
	ctrl.resident["bonsai"] = true
	m := newTestManager(ctrl)

	var calls int32
	m.preflight = func(backend string, _ status.SystemMetrics) (bool, string) {
		atomic.AddInt32(&calls, 1)
		return true, "image_missing" // would fail the test if ever reached
	}

	out, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Ready {
		t.Fatal("expected the resident engine to report ready immediately")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("preflight must not be called on the resident-hit fast path, got %d calls", got)
	}
	if ctrl.startCountVal() != 0 {
		t.Errorf("a resident engine must not be (re)started, got %d starts", ctrl.startCountVal())
	}
}

// TestSwap_PreflightUsesDiskMetricsFn asserts EnsureResident feeds the
// SwapManager's own diskMetrics reading (not a full status collection) into
// the preflight, so a stubbed disk-pressure reading reaches it end to end.
func TestSwap_PreflightUsesDiskMetricsFn(t *testing.T) {
	ctrl := newMockController()
	m := newTestManager(ctrl)
	m.waitBudget = 2 * time.Second
	m.diskMetrics = func() status.SystemMetrics {
		return status.SystemMetrics{DiskTotalGB: 500, DiskAvailableGB: 0.1, DiskPercent: 99}
	}
	// Use the real composition (status.EngineServeablePreflight) rather than a
	// stub, so this test actually exercises the diskMetrics wiring, not just
	// that some function got called.
	m.preflight = defaultSwapPreflight

	_, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	var preflightErr *SwapPreflightBlockedError
	if !errors.As(err, &preflightErr) {
		t.Fatalf("expected a SwapPreflightBlockedError from the injected disk-pressure reading, got %v", err)
	}
	if preflightErr.Reason != "disk_pressure" {
		t.Errorf("Reason = %q, want disk_pressure", preflightErr.Reason)
	}
}

// TestSwap_LedgerRecordsPreflightBlockedRefusal mirrors
// TestSwap_LedgerRecordsRateLimitedRefusal (swap_rate_test.go): a preflight
// refusal must be recorded distinctly, not folded into a generic "failed".
func TestSwap_LedgerRecordsPreflightBlockedRefusal(t *testing.T) {
	ctrl := newMockController()
	m := newTestManager(ctrl)
	m.waitBudget = 2 * time.Second
	m.preflight = func(backend string, _ status.SystemMetrics) (bool, string) {
		return true, "weights_missing"
	}

	if _, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B"); err == nil {
		t.Fatal("expected the swap to be refused")
	}
	stats := m.SwapStats()
	if len(stats.Recent) == 0 {
		t.Fatal("expected a ledger record")
	}
	last := stats.Recent[len(stats.Recent)-1]
	if last.Outcome != swapOutcomePreflightBlocked {
		t.Errorf("expected outcome %q, got %q", swapOutcomePreflightBlocked, last.Outcome)
	}
	if last.Evicting() {
		t.Error("a preflight-blocked swap evicted nothing")
	}
}

// TestSwapPreflightBlockedError_Error pins the error message names both the
// backend and the reason, so a log line is actionable without decoding a
// struct.
// TestHotswap_PreflightBlocked_FailsWithReason is the handler-side half of
// #956, mirroring TestHotswap_RateLimited_FailsWithReason
// (swap_rate_test.go): a SwapPreflightBlockedError from the swapper must
// become a job FAILURE carrying the preflight's own reason, never a
// model_warming success — warming promises the model is coming soon, which is
// false for a genuinely unserveable engine.
func TestHotswap_PreflightBlocked_FailsWithReason(t *testing.T) {
	h := NewLLMInferenceHandler().WithSwapper(&fakeSwapper{
		err: &SwapPreflightBlockedError{Backend: "bonsai", Reason: "image_missing"},
	})

	result, err := h.Execute(context.Background(), hotswapJob(), &MockStreamWriter{})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.Status != JobStatusFailure {
		t.Fatalf("a preflight block must be a job FAILURE, got %v", result.Status)
	}
	if got, _ := result.Output["reason"].(string); got != "image_missing" {
		t.Errorf("reason = %q, want image_missing", got)
	}
	if got, _ := result.Output["status"].(string); got != "model_unavailable" {
		t.Errorf("status = %q, want model_unavailable", got)
	}
	if got, _ := result.Output["error"].(string); got == "" {
		t.Error("a refusal must carry a human-readable explanation")
	}
}

func TestSwapPreflightBlockedError_Error(t *testing.T) {
	err := &SwapPreflightBlockedError{Backend: "bonsai", Reason: "disk_pressure"}
	msg := err.Error()
	if !strings.Contains(msg, "bonsai") || !strings.Contains(msg, "disk_pressure") {
		t.Errorf("Error() = %q, want it to name both the backend and the reason", msg)
	}
}
