package cmd

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// stubSwapController is a minimal worker.SwapController for the adapter test:
// it reports every backend already resident, so EnsureResident takes its
// fast, no-preemption-needed path and none of the other methods are ever
// exercised.
type stubSwapController struct{}

func (stubSwapController) Resident(_ context.Context, _ string) bool { return true }
func (stubSwapController) PreemptInputs(_ context.Context, _ string) ([]status.PreemptCandidate, uint64, bool) {
	return nil, 0, false
}
func (stubSwapController) StopNonDurable(_ string) error              { return nil }
func (stubSwapController) Start(_ context.Context, _, _ string) error { return nil }
func (stubSwapController) Ready(_ context.Context, _ string) bool     { return true }
func (stubSwapController) MeasuredVRAM(_ context.Context, _ string) (uint64, bool) {
	return 0, false
}

// TestSwapManagerAdapter_NotAvailableBeforeStore pins the construction-order
// fix (citadel-cli#686) at the point that used to be impossible: the adapter
// must be constructible and safely callable BEFORE the underlying
// atomic.Pointer[worker.SwapManager] has been populated — exactly the window
// between gateway construction (where SetModelSwapper is now called) and
// buildNodeJobHandlers (which populates nodeSwapManager later in runWork).
func TestSwapManagerAdapter_NotAvailableBeforeStore(t *testing.T) {
	var ptr atomic.Pointer[worker.SwapManager]
	adapter := newSwapManagerAdapter(&ptr)

	err := adapter.EnsureResident(context.Background(), "vllm", "some-model")
	if err == nil {
		t.Fatal("expected an error before the swap manager is populated, got nil")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Fatalf("error = %v, want a clear \"not available\" message, not a nil-pointer panic", err)
	}
}

// TestSwapManagerAdapter_UsesLatestStoredManager is the other half: once
// nodeSwapManager.Store runs (later in runWork, from buildNodeJobHandlers),
// the SAME adapter reference handed to the gateway at construction time must
// transparently start forwarding to the real manager — proving the
// construction-order fix actually closes the gap rather than just deferring
// the panic.
func TestSwapManagerAdapter_UsesLatestStoredManager(t *testing.T) {
	var ptr atomic.Pointer[worker.SwapManager]
	adapter := newSwapManagerAdapter(&ptr)

	mgr := worker.NewSwapManager(stubSwapController{})
	ptr.Store(mgr)

	if err := adapter.EnsureResident(context.Background(), "vllm", "some-model"); err != nil {
		t.Fatalf("EnsureResident() error = %v, want nil once the manager is populated", err)
	}
}
