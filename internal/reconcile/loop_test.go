package reconcile

import (
	"context"
	"testing"
	"time"
)

// TestReconcileOnceFullFlow drives Fetch -> Reconcile -> Apply -> Report through
// a FakeProvider + fakeOps and asserts the provider received an actual-state
// report reflecting the converged node.
func TestReconcileOnceFullFlow(t *testing.T) {
	ctx := context.Background()
	prov := &FakeProvider{Desired: ds(
		ModuleAssignment{Name: "foo", Source: "foo", DesiredStatus: StatusRunning},
	)}
	ops := newFakeOps()
	rec := NewReconciler(prov, ops, "node-x")

	plan, res, err := rec.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if res.Failed() {
		t.Fatalf("apply errors: %v", res.Errors)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Action != ActionInstall {
		t.Fatalf("unexpected plan: %v", planActions(plan))
	}
	if len(prov.Reported) != 1 {
		t.Fatalf("expected 1 report, got %d", len(prov.Reported))
	}
	report := prov.Reported[0]
	if report.Node != "node-x" {
		t.Errorf("report node = %q, want node-x", report.Node)
	}
	if len(report.Modules) != 1 || report.Modules[0].Name != "foo" ||
		report.Modules[0].Health != HealthRunning {
		t.Errorf("unexpected report modules: %+v", report.Modules)
	}

	// Second ReconcileOnce is a no-op (idempotent) but still reports.
	plan2, _, err := rec.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("2nd ReconcileOnce: %v", err)
	}
	if !plan2.IsEmpty() {
		t.Errorf("2nd pass should be empty, got %v", planActions(plan2))
	}
	if len(prov.Reported) != 2 {
		t.Errorf("expected 2 reports, got %d", len(prov.Reported))
	}
}

// TestReconcileOnceFetchError surfaces a provider fetch failure as a pass-level
// error (and performs no ops).
func TestReconcileOnceFetchError(t *testing.T) {
	prov := &FakeProvider{FetchErr: errf("control plane down")}
	ops := newFakeOps()
	rec := NewReconciler(prov, ops, "n")
	if _, _, err := rec.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("expected fetch error")
	}
	if len(ops.calls) != 0 {
		t.Errorf("no ops should run on fetch failure, got %v", ops.calls)
	}
}

// TestLoopDisabledByDefault asserts the zero-value Config disables the loop:
// Run returns immediately and never reconciles.
func TestLoopDisabledByDefault(t *testing.T) {
	prov := &FakeProvider{Desired: ds(ModuleAssignment{Name: "foo", Source: "foo"})}
	ops := newFakeOps()
	rec := NewReconciler(prov, ops, "n")

	loop := NewLoop(Config{}, rec) // Enabled defaults false
	err := loop.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("disabled loop Run should return nil, got %v", err)
	}
	if len(ops.calls) != 0 {
		t.Errorf("disabled loop must not reconcile, got %v", ops.calls)
	}
	if len(prov.Reported) != 0 {
		t.Errorf("disabled loop must not report, got %d", len(prov.Reported))
	}
}

// TestLoopNudgeRunsPass asserts a push nudge triggers a pass when enabled, and
// that the loop stops on context cancellation.
func TestLoopNudgeRunsPass(t *testing.T) {
	prov := &FakeProvider{Desired: ds(ModuleAssignment{Name: "foo", Source: "foo"})}
	ops := newFakeOps()
	rec := NewReconciler(prov, ops, "n")

	// Long interval so the ticker never fires during the test; the pass comes
	// only from the nudge. MinInterval tiny so debounce never blocks.
	loop := NewLoop(Config{Enabled: true, Interval: time.Hour, MinInterval: time.Nanosecond}, rec)

	done := make(chan struct{})
	passed := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = loop.Run(ctx, func(_ Plan, _ ApplyResult, _ error) {
			select {
			case passed <- struct{}{}:
			default:
			}
		})
		close(done)
	}()

	loop.Nudge()
	select {
	case <-passed:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("nudge did not trigger a pass")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop on cancel")
	}
	if len(prov.Reported) == 0 {
		t.Error("expected at least one report after nudge")
	}
}

// TestLoopDebounce uses an injectable clock to assert two passes closer than
// MinInterval are debounced (the second allow() returns false).
func TestLoopDebounce(t *testing.T) {
	loop := NewLoop(Config{Enabled: true, MinInterval: time.Minute}, nil)
	base := time.Unix(1000, 0)
	cur := base
	loop.now = func() time.Time { return cur }

	if !loop.allow() {
		t.Fatal("first pass should be allowed")
	}
	cur = base.Add(30 * time.Second) // within MinInterval
	if loop.allow() {
		t.Fatal("second pass within MinInterval should be debounced")
	}
	cur = base.Add(2 * time.Minute) // past MinInterval
	if !loop.allow() {
		t.Fatal("pass after MinInterval should be allowed")
	}
}

// TestHandleReconcileJobInertWhenDisabled asserts the push-nudge handler does
// nothing when the feature is disabled.
func TestHandleReconcileJobInertWhenDisabled(t *testing.T) {
	prov := &FakeProvider{Desired: ds(ModuleAssignment{Name: "foo", Source: "foo"})}
	ops := newFakeOps()
	rec := NewReconciler(prov, ops, "n")

	if err := HandleReconcileJob(context.Background(), Config{}, nil, rec); err != nil {
		t.Fatalf("inert handler should return nil, got %v", err)
	}
	if len(ops.calls) != 0 {
		t.Errorf("disabled handler must not reconcile, got %v", ops.calls)
	}
}

// TestHandleReconcileJobEnabledRunsOnce asserts the handler runs a single pass
// when enabled and given a reconciler (no loop).
func TestHandleReconcileJobEnabledRunsOnce(t *testing.T) {
	prov := &FakeProvider{Desired: ds(ModuleAssignment{Name: "foo", Source: "foo"})}
	ops := newFakeOps()
	rec := NewReconciler(prov, ops, "n")

	if err := HandleReconcileJob(context.Background(), Config{Enabled: true}, nil, rec); err != nil {
		t.Fatalf("enabled handler: %v", err)
	}
	if len(prov.Reported) != 1 {
		t.Errorf("expected one reconcile pass, got %d reports", len(prov.Reported))
	}
}

// TestHandleReconcileJobEnabledNudgesLoop asserts the handler nudges a provided
// loop instead of running synchronously.
func TestHandleReconcileJobEnabledNudgesLoop(t *testing.T) {
	loop := NewLoop(Config{Enabled: true}, nil)
	if err := HandleReconcileJob(context.Background(), Config{Enabled: true}, loop, nil); err != nil {
		t.Fatalf("handler with loop: %v", err)
	}
	// The nudge should be buffered in the channel.
	select {
	case <-loop.nudg:
	default:
		t.Error("expected a buffered nudge")
	}
}
