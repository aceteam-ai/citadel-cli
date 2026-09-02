package gateway

import (
	"context"
	"testing"
)

// fakeModelSwapper is a minimal ModelSwapper for tests; it just proves it was
// reached and records every call. outcome/err let a test configure what
// EnsureResident reports (a warming outcome, a ready outcome, or a failure);
// the zero value reports SwapOutcome{Ready: false} (i.e. still warming) with
// no error, which is the honest zero value, not a stand-in for success.
type fakeModelSwapper struct {
	calls   []string
	outcome SwapOutcome
	err     error
}

func (f *fakeModelSwapper) EnsureResident(_ context.Context, backend, model string) (SwapOutcome, error) {
	f.calls = append(f.calls, backend+"/"+model)
	if f.err != nil {
		return SwapOutcome{}, f.err
	}
	return f.outcome, nil
}

// TestModelSwapper_DefaultsToNil pins the pre-#686 behavior: a gateway with no
// swapper wired has a nil modelSwapper, so any future caller that checks it
// before using it degrades safely (no swap attempted) rather than panicking on
// a nil interface method call.
func TestModelSwapper_DefaultsToNil(t *testing.T) {
	gw := NewServer(Config{Port: 0, NodeName: "test-node"})
	if gw.modelSwapper != nil {
		t.Fatalf("modelSwapper = %v, want nil before SetModelSwapper", gw.modelSwapper)
	}
}

// TestSetModelSwapper_ReachableFromChatRoutePath pins citadel-cli#686's
// construction-order fix: a swapper wired via SetModelSwapper — including
// BEFORE registerChatRoutes runs, matching how cmd/work.go wires it at
// gateway-construction time, well before the swap manager itself exists —
// must be the exact same reference the chat-route path (s.modelSwapper, which
// handleChatCompletions would read once #686 wires the fallback in) observes.
// Before this fix there was no field/setter for this at all: the gateway had
// no way to reach the swapper the job path already used.
func TestSetModelSwapper_ReachableFromChatRoutePath(t *testing.T) {
	gw := NewServer(Config{Port: 0, NodeName: "test-node"})
	swapper := &fakeModelSwapper{}

	gw.SetModelSwapper(swapper)
	gw.SetChatRouter(func() []ChatUpstream { return nil })
	gw.registerChatRoutes()

	got := gw.modelSwapper
	if got == nil {
		t.Fatal("modelSwapper = nil after SetModelSwapper, want the wired swapper")
	}
	if got != ModelSwapper(swapper) {
		t.Fatalf("modelSwapper holds a different reference than the one set")
	}
	// Exercise it through the interface the way a future caller would, to
	// confirm it is a live, callable reference, not just a stored pointer.
	if _, err := got.EnsureResident(context.Background(), "vllm", "some-model"); err != nil {
		t.Fatalf("EnsureResident() error = %v", err)
	}
	if len(swapper.calls) != 1 || swapper.calls[0] != "vllm/some-model" {
		t.Fatalf("swapper.calls = %v, want one call for vllm/some-model", swapper.calls)
	}
}

// TestSetModelSwapper_SafeBeforeSwapManagerExists is the direct regression
// pin for the bug: SetModelSwapper must be usable at gateway-construction
// time (before any real *worker.SwapManager exists), because that is
// EXACTLY when cmd/work.go now calls it — well before buildNodeJobHandlers
// constructs the real swap manager. A late-binding adapter (wrapping a
// pointer that starts nil and is populated afterward) is how cmd/ satisfies
// this without reordering the existing startup sequence; this test pins the
// gateway side of that contract: setting, then later replacing, the wired
// swapper must both be observable.
func TestSetModelSwapper_SafeBeforeSwapManagerExists(t *testing.T) {
	gw := NewServer(Config{Port: 0, NodeName: "test-node"})

	placeholder := &fakeModelSwapper{}
	gw.SetModelSwapper(placeholder)
	if gw.modelSwapper != ModelSwapper(placeholder) {
		t.Fatal("expected the placeholder swapper to be wired immediately")
	}

	real := &fakeModelSwapper{}
	gw.SetModelSwapper(real)
	if gw.modelSwapper != ModelSwapper(real) {
		t.Fatal("expected SetModelSwapper to replace the wired swapper")
	}
}
