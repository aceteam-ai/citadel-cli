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
	// The guard blocks the APPLY, not the report (#733). Reporting is the
	// observability path: suppressing it left the control plane unable to see the
	// installed set it was refusing to wipe. Exactly one report per refused pass.
	if len(provider.Reported) != 1 {
		t.Fatalf("guard must still report actual state, got %d reports", len(provider.Reported))
	}
}

// TestRefuseFullWipeStillReportsActualState is the #733 regression: a refused
// pass must still tell the control plane what the node has installed. Before the
// fix the guard returned before BuildActualState/Report, so a node that tripped
// it reported nothing about its modules for as long as the condition held.
func TestRefuseFullWipeStillReportsActualState(t *testing.T) {
	ops := newFakeOps(
		InstalledModule{Name: "a", Source: "src-a", Health: HealthRunning},
		InstalledModule{Name: "b", Source: "src-b", Health: HealthStopped},
	)
	provider := &FakeProvider{Desired: DesiredState{Revision: "rev-empty"}} // zero modules
	rec := NewReconciler(provider, ops, "node-7")
	rec.RefuseFullWipe = true

	if _, _, err := rec.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("expected refusal error for empty desired with modules installed")
	}

	if len(provider.Reported) != 1 {
		t.Fatalf("want exactly 1 report from the refused pass, got %d", len(provider.Reported))
	}
	report := provider.Reported[0]
	if report.Node != "node-7" {
		t.Errorf("report node = %q, want %q", report.Node, "node-7")
	}
	if len(report.Modules) != 2 {
		t.Fatalf("want 2 reported modules, got %d (%+v)", len(report.Modules), report.Modules)
	}
	got := map[string]InstalledModule{}
	for _, m := range report.Modules {
		got[m.Name] = m
	}
	if m, ok := got["a"]; !ok || m.Source != "src-a" || m.Health != HealthRunning {
		t.Errorf("module a reported as %+v, want source src-a health running", m)
	}
	if m, ok := got["b"]; !ok || m.Source != "src-b" || m.Health != HealthStopped {
		t.Errorf("module b reported as %+v, want source src-b health stopped", m)
	}
}

// TestRefuseFullWipeReportOmitsAppliedRevision pins the handshake half of the
// fix: a refused pass must NOT claim it applied the revision it refused. The
// converge path stamps AppliedRevision; the guard path deliberately does not, or
// the control plane would record a convergence that never happened.
func TestRefuseFullWipeReportOmitsAppliedRevision(t *testing.T) {
	ops := newFakeOps(InstalledModule{Name: "a", Source: "src-a", Health: HealthRunning})
	provider := &FakeProvider{Desired: DesiredState{Revision: "rev-42"}} // zero modules
	rec := NewReconciler(provider, ops, "node-7")
	rec.RefuseFullWipe = true

	if _, _, err := rec.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("expected refusal error")
	}
	if len(provider.Reported) != 1 {
		t.Fatalf("want exactly 1 report, got %d", len(provider.Reported))
	}
	if rev := provider.Reported[0].AppliedRevision; rev != "" {
		t.Errorf("refused pass reported AppliedRevision %q, want empty (nothing was applied)", rev)
	}
}

// TestRefuseFullWipeSurfacesReportFailure confirms a failed report on the guard
// path does not swallow either error: the refusal stays visible (it is the
// operator-actionable one) and the report failure is surfaced alongside it, so a
// node that is refusing AND unable to report is not mistaken for one that is
// merely refusing.
func TestRefuseFullWipeSurfacesReportFailure(t *testing.T) {
	ops := newFakeOps(InstalledModule{Name: "a", Source: "src-a", Health: HealthRunning})
	provider := &FakeProvider{
		Desired:   DesiredState{Revision: "rev-empty"},
		ReportErr: errf("post node-state: 503"),
	}
	rec := NewReconciler(provider, ops, "node-7")
	rec.RefuseFullWipe = true

	_, _, err := rec.ReconcileOnce(context.Background())
	if err == nil {
		t.Fatal("expected an error when both the guard trips and the report fails")
	}
	if !strings.Contains(err.Error(), "refusing empty desired state") {
		t.Errorf("refusal must stay visible, got: %v", err)
	}
	if !strings.Contains(err.Error(), "post node-state: 503") {
		t.Errorf("report failure must be surfaced, got: %v", err)
	}
	for _, c := range ops.calls {
		if strings.HasPrefix(c, "uninstall:") {
			t.Fatalf("a failed report must not unblock the apply, saw %q", c)
		}
	}
}

// TestRefuseFullWipeSkipsNeverManagedNode is the #733 root-cause regression: a
// node the control plane has NEVER assigned any desired state to (Revision ==
// "0", the zero value the control plane serves when it holds no
// fabric_node_module_desired row for this node at all) must not be treated as
// "misconfigured". Before this fix EVERY node hit this path forever, because
// nothing writes that durable store yet: the guard fired and errored on every
// single pass (4574+ times over 5+ days, observed on node 1297). It must now
// succeed as a no-op converge (nothing installed, nothing uninstalled) while
// still reporting the observed actual state, exactly like a legitimate
// converged pass.
func TestRefuseFullWipeSkipsNeverManagedNode(t *testing.T) {
	ops := newFakeOps(
		InstalledModule{Name: "a", Source: "src-a", Health: HealthRunning},
		InstalledModule{Name: "b", Source: "src-b", Health: HealthRunning},
	)
	provider := &FakeProvider{Desired: DesiredState{Revision: "0"}} // never managed
	rec := NewReconciler(provider, ops, "node-1297")
	rec.RefuseFullWipe = true

	plan, _, err := rec.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("never-managed node with modules installed must succeed as a no-op, got: %v", err)
	}
	if !plan.IsEmpty() {
		t.Errorf("want empty plan (nothing to converge to), got %+v", plan.Steps)
	}
	for _, c := range ops.calls {
		if strings.HasPrefix(c, "uninstall:") {
			t.Fatalf("a never-managed node must not uninstall anything, saw %q", c)
		}
	}
	if len(provider.Reported) != 1 {
		t.Fatalf("want exactly 1 report, got %d", len(provider.Reported))
	}
	report := provider.Reported[0]
	if len(report.Modules) != 2 {
		t.Fatalf("want 2 reported modules, got %d (%+v)", len(report.Modules), report.Modules)
	}
	// Unlike the refused-and-errored path, this IS a (no-op) converge, so the
	// revision handshake stamps normally.
	if report.AppliedRevision != "0" {
		t.Errorf("AppliedRevision = %q, want %q (echoed like any other converge)", report.AppliedRevision, "0")
	}
}

// TestRefuseFullWipeSkipsNeverManagedNodeEmptyRevision confirms the same
// no-op behavior when Revision is the empty string, not just the literal "0":
// both are DesiredState.NeverManaged().
func TestRefuseFullWipeSkipsNeverManagedNodeEmptyRevision(t *testing.T) {
	ops := newFakeOps(InstalledModule{Name: "a", Source: "src-a", Health: HealthRunning})
	provider := &FakeProvider{Desired: DesiredState{Revision: ""}}
	rec := NewReconciler(provider, ops, "node")
	rec.RefuseFullWipe = true

	plan, _, err := rec.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("never-managed node (empty revision) must succeed as a no-op, got: %v", err)
	}
	if !plan.IsEmpty() {
		t.Errorf("want empty plan, got %+v", plan.Steps)
	}
}

// TestDesiredStateNeverManaged pins the predicate directly: only the true
// zero-history values ("" and "0") count as never-managed; any other revision,
// including a non-numeric opaque token (which is all the wire contract
// promises), means the control plane has a history for this node.
func TestDesiredStateNeverManaged(t *testing.T) {
	cases := []struct {
		revision string
		want     bool
	}{
		{"", true},
		{"0", true},
		{"1", false},
		{"rev-42", false},
		{"1784231386757", false},
	}
	for _, c := range cases {
		got := DesiredState{Revision: c.revision}.NeverManaged()
		if got != c.want {
			t.Errorf("DesiredState{Revision: %q}.NeverManaged() = %v, want %v", c.revision, got, c.want)
		}
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
