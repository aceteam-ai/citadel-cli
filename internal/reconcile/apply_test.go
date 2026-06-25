package reconcile

import (
	"context"
	"strings"
	"testing"
)

// TestApplyConvergesAndIsIdempotent runs Reconcile -> Apply -> Reconcile and
// asserts the SECOND plan is empty (the heart of idempotency). The fake ops are
// stateful, so the second ListInstalled reflects the first apply.
func TestApplyConvergesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	ops := newFakeOps(
		// pre-existing: one stale (to uninstall), one drifted (to update),
		// one that just needs starting.
		InstalledModule{Name: "stale", Source: "stale", Health: HealthRunning},
		InstalledModule{Name: "drift", Source: "drift@v1", Health: HealthRunning},
		InstalledModule{Name: "down", Source: "down", Health: HealthStopped},
	)
	desired := ds(
		ModuleAssignment{Name: "fresh", Source: "fresh", DesiredStatus: StatusRunning},
		ModuleAssignment{Name: "drift", Source: "drift@v2", DesiredStatus: StatusRunning},
		ModuleAssignment{Name: "down", Source: "down", DesiredStatus: StatusRunning},
		ModuleAssignment{Name: "parked", Source: "parked", DesiredStatus: StatusStopped},
	)

	// First pass.
	actual, _ := ops.ListInstalled(ctx)
	plan, err := Reconcile(ctx, desired, actual)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if plan.IsEmpty() {
		t.Fatal("first plan should not be empty")
	}
	res, err := Apply(ctx, ops, plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Failed() {
		t.Fatalf("unexpected apply errors: %v", res.Errors)
	}

	// Verify the converged state matches desired.
	got, _ := ops.ListInstalled(ctx)
	gotByName := map[string]InstalledModule{}
	for _, im := range got {
		gotByName[im.Name] = im
	}
	if _, ok := gotByName["stale"]; ok {
		t.Error("stale should have been uninstalled")
	}
	if gotByName["drift"].Source != "drift@v2" {
		t.Errorf("drift source = %q, want drift@v2", gotByName["drift"].Source)
	}
	if gotByName["down"].Health != HealthRunning {
		t.Errorf("down health = %q, want running", gotByName["down"].Health)
	}
	if gotByName["parked"].Health != HealthStopped {
		t.Errorf("parked health = %q, want stopped", gotByName["parked"].Health)
	}
	if gotByName["fresh"].Health != HealthRunning {
		t.Errorf("fresh health = %q, want running", gotByName["fresh"].Health)
	}

	// Second pass: MUST be a no-op.
	actual2, _ := ops.ListInstalled(ctx)
	plan2, err := Reconcile(ctx, desired, actual2)
	if err != nil {
		t.Fatalf("Reconcile (2nd): %v", err)
	}
	if !plan2.IsEmpty() {
		t.Fatalf("second plan should be empty (idempotency), got: %v", planActions(plan2))
	}
}

// TestApplyPerModuleFailureIsolation asserts one failing module records an error
// and does NOT abort the others, and that the error surfaces in the actual-state
// report (Health == HealthError with the message).
func TestApplyPerModuleFailureIsolation(t *testing.T) {
	ctx := context.Background()
	ops := newFakeOps()
	// "bad" install fails; "good1"/"good2" succeed.
	ops.failModule("bad", "install", errf("boom: registry unreachable"))

	desired := ds(
		ModuleAssignment{Name: "good1", Source: "good1", DesiredStatus: StatusRunning},
		ModuleAssignment{Name: "bad", Source: "bad", DesiredStatus: StatusRunning},
		ModuleAssignment{Name: "good2", Source: "good2", DesiredStatus: StatusRunning},
	)

	actual, _ := ops.ListInstalled(ctx)
	plan, err := Reconcile(ctx, desired, actual)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	res, err := Apply(ctx, ops, plan)
	if err != nil {
		t.Fatalf("Apply returned pass-level error: %v", err)
	}
	if !res.Failed() {
		t.Fatal("expected at least one per-module failure")
	}
	if _, ok := res.Errors["bad"]; !ok {
		t.Error("bad module error not recorded")
	}
	if _, ok := res.Errors["good1"]; ok {
		t.Error("good1 should not have errored")
	}

	// The good modules were still installed despite bad's failure.
	got, _ := ops.ListInstalled(ctx)
	names := map[string]bool{}
	for _, im := range got {
		names[im.Name] = true
	}
	if !names["good1"] || !names["good2"] {
		t.Errorf("good modules not installed despite isolation; have %v", names)
	}
	if names["bad"] {
		t.Error("bad module should not be installed after a failed install")
	}

	// The failure surfaces in the actual-state report.
	report, err := BuildActualState(ctx, ops, res.Errors, "node-1")
	if err != nil {
		t.Fatalf("BuildActualState: %v", err)
	}
	var badReport *InstalledModule
	for i := range report.Modules {
		if report.Modules[i].Name == "bad" {
			badReport = &report.Modules[i]
		}
	}
	if badReport == nil {
		t.Fatal("bad module missing from actual-state report")
	}
	if badReport.Health != HealthError {
		t.Errorf("bad health = %q, want error", badReport.Health)
	}
	if !strings.Contains(badReport.Error, "registry unreachable") {
		t.Errorf("bad error = %q, want it to mention the failure", badReport.Error)
	}
	if report.Node != "node-1" {
		t.Errorf("report node = %q, want node-1", report.Node)
	}
}

// TestApplyFailureIsolationOnExisting verifies an error on an installed module
// (e.g. a stop that fails) is reported as HealthError while still listing the
// module, and other modules are unaffected.
func TestApplyFailureIsolationOnExisting(t *testing.T) {
	ctx := context.Background()
	ops := newFakeOps(
		InstalledModule{Name: "stuck", Source: "stuck", Health: HealthRunning},
		InstalledModule{Name: "fine", Source: "fine", Health: HealthRunning},
	)
	ops.failModule("stuck", "stop", errf("container will not stop"))

	desired := ds(
		ModuleAssignment{Name: "stuck", Source: "stuck", DesiredStatus: StatusStopped},
		ModuleAssignment{Name: "fine", Source: "fine", DesiredStatus: StatusRunning},
	)
	actual, _ := ops.ListInstalled(ctx)
	plan, _ := Reconcile(ctx, desired, actual)
	res, err := Apply(ctx, ops, plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, ok := res.Errors["stuck"]; !ok {
		t.Fatal("stuck stop failure not recorded")
	}

	report, _ := BuildActualState(ctx, ops, res.Errors, "")
	byName := map[string]InstalledModule{}
	for _, im := range report.Modules {
		byName[im.Name] = im
	}
	if byName["stuck"].Health != HealthError {
		t.Errorf("stuck health = %q, want error", byName["stuck"].Health)
	}
	// "fine" stays running and error-free.
	if byName["fine"].Health == HealthError {
		t.Error("fine should not be in error")
	}
}
