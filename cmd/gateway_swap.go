package cmd

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// gateway_swap.go is the construction-order fix for citadel-cli#686 (see
// docs/design-engine-adapter.md §4): a *worker.SwapManager is constructed
// inside buildNodeJobHandlers, but that happens well after the gateway's chat
// router is wired (SetChatRouter) and after gw.Start has already been kicked
// off in its own goroutine (cmd/work.go's runWork). There was no point in the
// existing startup sequence at which a caller could hand the gateway a valid
// swapper reference — the capability existed in the same process but was
// unreachable from the gateway.
//
// swapManagerAdapter closes that gap WITHOUT reordering the startup sequence:
// it wraps the SAME atomic.Pointer[worker.SwapManager] the heartbeat's swap-
// stats reporting already reads (runWork's nodeSwapManager, see its doc
// comment for why it is an atomic.Pointer and not a plain var), so it can be
// constructed and handed to gw.SetModelSwapper immediately at gateway-
// construction time — before nodeSwapManager has been populated — and it
// resolves to the real manager the moment buildNodeJobHandlers later calls
// nodeSwapManager.Store. internal/gateway defines the ModelSwapper interface
// with gateway-owned types specifically so it never needs to import
// internal/worker; this adapter is what bridges the two, living in cmd/ (which
// already imports both).
//
// Scope: this makes a swapper reference REACHABLE from the gateway. It does
// NOT make the chat route call EnsureResident — wiring that into
// resolveChatModel's routing decision, plus the model_warming response
// contract for a still-warming swap, is #686's larger, deferred scope (see the
// SetModelSwapper doc comment in internal/gateway/chat_route.go).
type swapManagerAdapter struct {
	mgr *atomic.Pointer[worker.SwapManager]
}

// newSwapManagerAdapter builds a gateway.ModelSwapper backed by mgr. mgr may
// still be nil-valued (unpopulated) at construction time; EnsureResident
// re-reads it on every call, so it transparently starts working once
// buildNodeJobHandlers populates it.
func newSwapManagerAdapter(mgr *atomic.Pointer[worker.SwapManager]) gateway.ModelSwapper {
	return &swapManagerAdapter{mgr: mgr}
}

func (a *swapManagerAdapter) EnsureResident(ctx context.Context, backend, model string) error {
	m := a.mgr.Load()
	if m == nil {
		// Hotswap disabled (break-glass), no config dir, or called before
		// buildNodeJobHandlers has run yet. Not currently reachable (nothing
		// calls EnsureResident on the gateway path yet — see the package doc
		// comment), but a clear error rather than a nil-pointer panic is the
		// right failure mode for whenever #686 wires a real caller.
		return fmt.Errorf("model swap manager not available on this node")
	}
	_, err := m.EnsureResident(ctx, backend, model)
	return err
}
