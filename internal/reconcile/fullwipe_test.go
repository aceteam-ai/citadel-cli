package reconcile

import (
	"context"
	"strings"
	"testing"
)

// TestRefuseFullWipeBlocksEmptyDesiredWithInstalled asserts the safety belt: an
// empty desired set while modules are installed is refused, and NOTHING is
// uninstalled (the empty/misconfigured-backend foot-gun).
func TestRefuseFullWipeBlocksEmptyDesiredWithInstalled(t *testing.T) {
	ops := newFakeOps(
		InstalledModule{Name: "a", Source: "a", Health: HealthRunning},
		InstalledModule{Name: "b", Source: "b", Health: HealthRunning},
	)
	provider := &FakeProvider{Desired: DesiredState{Revision: "rev-empty"}} // zero modules
	rec := NewReconciler(provider, ops, "node")
	rec.RefuseFullWipe = true

	_, _, err := rec.ReconcileOnce(context.Background())
	if err == nil {
		t.Fatal("expected refusal error for empty desired with modules installed")
	}
	if !strings.Contains(err.Error(), "refusing empty desired state") {
		t.Errorf("unexpected error: %v", err)
	}
	for _, c := range ops.calls {
		if strings.HasPrefix(c, "uninstall:") {
			t.Fatalf("full-wipe guard must not uninstall anything, saw %q", c)
		}
	}
	if len(provider.Reported) != 0 {
		t.Errorf("guard must abort before reporting, got %d reports", len(provider.Reported))
	}
}

// TestRefuseFullWipeAllowsEmptyDesiredWhenNothingInstalled confirms the guard is
// scoped: an empty desired set on an empty node is a legitimate no-op converge.
func TestRefuseFullWipeAllowsEmptyDesiredWhenNothingInstalled(t *testing.T) {
	ops := newFakeOps()
	provider := &FakeProvider{Desired: DesiredState{}}
	rec := NewReconciler(provider, ops, "node")
	rec.RefuseFullWipe = true

	plan, _, err := rec.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("empty desired on empty node must succeed: %v", err)
	}
	if !plan.IsEmpty() {
		t.Errorf("want empty plan, got %+v", plan.Steps)
	}
}

// TestRefuseFullWipeDisabledKeepsAuthoritativeSemantics confirms the default
// (guard off) still lets the engine uninstall drift, unchanged.
func TestRefuseFullWipeDisabledKeepsAuthoritativeSemantics(t *testing.T) {
	ops := newFakeOps(InstalledModule{Name: "a", Source: "a", Health: HealthRunning})
	provider := &FakeProvider{Desired: DesiredState{}}
	rec := NewReconciler(provider, ops, "node") // RefuseFullWipe defaults false

	plan, _, err := rec.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Action != ActionUninstall {
		t.Fatalf("want a single uninstall step, got %+v", plan.Steps)
	}
}
