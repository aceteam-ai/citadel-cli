package worker

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/reconcile"
)

// fakeModuleOps is a STATEFUL in-memory reconcile.ModuleOps for handler tests.
// It is keyed by the module's real service name (which may differ from the
// source basename, so the name-gap / source-scoping behavior can be asserted).
// Every op is logged so tests can prove the handler routed a single assignment
// through the scoped engine WITHOUT touching the node's other modules.
type fakeModuleOps struct {
	byName  map[string]reconcile.InstalledModule
	calls   []string
	failOps map[string]error // "install"/"uninstall"/"start"/"stop"/"list" -> err
}

func newFakeModuleOps(seed ...reconcile.InstalledModule) *fakeModuleOps {
	f := &fakeModuleOps{byName: map[string]reconcile.InstalledModule{}, failOps: map[string]error{}}
	for _, im := range seed {
		f.byName[im.Name] = im
	}
	return f
}

func (f *fakeModuleOps) Install(ctx context.Context, m reconcile.ModuleAssignment) error {
	f.calls = append(f.calls, "install:"+m.Key())
	if err := f.failOps["install"]; err != nil {
		return err
	}
	// Fresh install => running, recording the requested source + config so a
	// re-list diffs equal against the same assignment (no churn).
	f.byName[m.Key()] = reconcile.InstalledModule{
		Name:   m.Key(),
		Source: m.Source,
		Config: m.Config,
		Health: reconcile.HealthRunning,
	}
	return nil
}

func (f *fakeModuleOps) Uninstall(ctx context.Context, name string) error {
	f.calls = append(f.calls, "uninstall:"+name)
	if err := f.failOps["uninstall"]; err != nil {
		return err
	}
	delete(f.byName, name)
	return nil
}

func (f *fakeModuleOps) Start(ctx context.Context, name string) error {
	f.calls = append(f.calls, "start:"+name)
	if err := f.failOps["start"]; err != nil {
		return err
	}
	if im, ok := f.byName[name]; ok {
		im.Health = reconcile.HealthRunning
		f.byName[name] = im
	}
	return nil
}

func (f *fakeModuleOps) Stop(ctx context.Context, name string) error {
	f.calls = append(f.calls, "stop:"+name)
	if err := f.failOps["stop"]; err != nil {
		return err
	}
	if im, ok := f.byName[name]; ok {
		im.Health = reconcile.HealthStopped
		f.byName[name] = im
	}
	return nil
}

func (f *fakeModuleOps) ListInstalled(ctx context.Context) ([]reconcile.InstalledModule, error) {
	if err := f.failOps["list"]; err != nil {
		return nil, err
	}
	out := make([]reconcile.InstalledModule, 0, len(f.byName))
	for _, im := range f.byName {
		out = append(out, im)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeModuleOps) names() []string {
	out := make([]string, 0, len(f.byName))
	for n := range f.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func moduleSetJob(sourceQueue string, assignment map[string]any) *Job {
	return &Job{ID: "ms-1", Type: JobTypeModuleSet, SourceQueue: sourceQueue, Payload: assignment}
}

func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

// The shared org pool (no ":node:" segment) must be refused fail-closed.
const sharedPoolQueue = "jobs:v1:shell:org_test"

func TestModuleSetRefusesSharedPool(t *testing.T) {
	f := newFakeModuleOps()
	h := NewModuleSetHandler(ModuleSetConfig{Ops: f})
	res, err := h.Execute(context.Background(), moduleSetJob(sharedPoolQueue, map[string]any{
		"source": "owner/repo@v1", "desired_status": "running",
	}), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure (fail-closed)", res.Status)
	}
	if len(f.calls) != 0 {
		t.Fatalf("shared-pool job must not touch ops; got calls %v", f.calls)
	}
}

func TestModuleSetInstallsMissingModule(t *testing.T) {
	f := newFakeModuleOps()
	h := NewModuleSetHandler(ModuleSetConfig{Ops: f})
	res, err := h.Execute(context.Background(), moduleSetJob(perNodeQueue, map[string]any{
		"source": "owner/repo@v1", "desired_status": "running",
	}), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %v, want success", res.Status)
	}
	if !hasCall(f.calls, "install:repo") {
		t.Fatalf("expected install:repo, got %v", f.calls)
	}
}

// The single-module scope must NOT uninstall the node's other modules.
func TestModuleSetDoesNotTouchOtherModules(t *testing.T) {
	f := newFakeModuleOps(
		reconcile.InstalledModule{Name: "alpha", Source: "owner/alpha@v1", Health: reconcile.HealthRunning},
		reconcile.InstalledModule{Name: "beta", Source: "owner/beta@v1", Health: reconcile.HealthRunning},
		reconcile.InstalledModule{Name: "target", Source: "owner/target@v1", Health: reconcile.HealthRunning},
	)
	h := NewModuleSetHandler(ModuleSetConfig{Ops: f})
	// Uninstall ONLY target.
	res, err := h.Execute(context.Background(), moduleSetJob(perNodeQueue, map[string]any{
		"source": "owner/target@v1", "desired_status": "absent",
	}), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %v, want success", res.Status)
	}
	if !hasCall(f.calls, "uninstall:target") {
		t.Fatalf("expected uninstall:target, got %v", f.calls)
	}
	got := f.names()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("other modules must survive; remaining = %v", got)
	}
	// Nothing else should have been uninstalled.
	if hasCall(f.calls, "uninstall:alpha") || hasCall(f.calls, "uninstall:beta") {
		t.Fatalf("scoped uninstall leaked to other modules: %v", f.calls)
	}
}

// absent uninstalls when installed, and is a no-op when already absent.
func TestModuleSetAbsentIsIdempotent(t *testing.T) {
	f := newFakeModuleOps(
		reconcile.InstalledModule{Name: "target", Source: "owner/target@v1", Health: reconcile.HealthRunning},
	)
	h := NewModuleSetHandler(ModuleSetConfig{Ops: f})

	// First absent: uninstalls.
	if _, err := h.Execute(context.Background(), moduleSetJob(perNodeQueue, map[string]any{
		"source": "owner/target@v1", "desired_status": "absent",
	}), &NoOpStreamWriter{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasCall(f.calls, "uninstall:target") {
		t.Fatalf("expected uninstall:target, got %v", f.calls)
	}

	// Second absent: no-op success, NO further uninstall.
	f.calls = nil
	res, err := h.Execute(context.Background(), moduleSetJob(perNodeQueue, map[string]any{
		"source": "owner/target@v1", "desired_status": "absent",
	}), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %v, want success", res.Status)
	}
	for _, c := range f.calls {
		if c == "uninstall:target" {
			t.Fatalf("re-absent must be a no-op, but uninstalled again: %v", f.calls)
		}
	}
}

// stopped stops (does NOT uninstall) and keeps the module installed -- the
// durable stopped state, distinct from absent. The name-gap is also covered
// here: the installed service name ("svc") differs from the source basename
// ("repo"), and the handler must align to "svc" so it stops (not churn-install).
func TestModuleSetStoppedStopsNotUninstalls(t *testing.T) {
	f := newFakeModuleOps(
		reconcile.InstalledModule{Name: "svc", Source: "owner/repo@v1", Health: reconcile.HealthRunning},
	)
	h := NewModuleSetHandler(ModuleSetConfig{Ops: f})
	res, err := h.Execute(context.Background(), moduleSetJob(perNodeQueue, map[string]any{
		"source": "owner/repo@v1", "desired_status": "stopped",
	}), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %v, want success", res.Status)
	}
	if !hasCall(f.calls, "stop:svc") {
		t.Fatalf("expected stop:svc (name aligned past the source basename), got %v", f.calls)
	}
	if hasCall(f.calls, "uninstall:svc") {
		t.Fatalf("stopped must NOT uninstall: %v", f.calls)
	}
	if hasCall(f.calls, "install:repo") || hasCall(f.calls, "install:svc") {
		t.Fatalf("stopped on a matching install must not churn-install: %v", f.calls)
	}
	im, ok := f.byName["svc"]
	if !ok {
		t.Fatalf("module must stay installed after stop")
	}
	if im.Health != reconcile.HealthStopped {
		t.Fatalf("health = %v, want stopped", im.Health)
	}
}

// A converged node (desired == actual) yields NO steps: proof of idempotency and
// no source-basename churn when the installed name differs from the basename.
func TestModuleSetRunningConvergedIsNoOp(t *testing.T) {
	f := newFakeModuleOps(
		reconcile.InstalledModule{Name: "svc", Source: "owner/repo@v1", Health: reconcile.HealthRunning},
	)
	h := NewModuleSetHandler(ModuleSetConfig{Ops: f})
	res, err := h.Execute(context.Background(), moduleSetJob(perNodeQueue, map[string]any{
		"source": "owner/repo@v1", "desired_status": "running",
	}), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %v, want success", res.Status)
	}
	for _, c := range f.calls {
		switch {
		case len(c) >= 8 && c[:8] == "install:",
			len(c) >= 10 && c[:10] == "uninstall:",
			len(c) >= 6 && c[:6] == "start:",
			len(c) >= 5 && c[:5] == "stop:":
			t.Fatalf("converged running module must produce no side effects, got %v", f.calls)
		}
	}
}

func TestModuleSetMissingSourceIsTerminal(t *testing.T) {
	f := newFakeModuleOps()
	h := NewModuleSetHandler(ModuleSetConfig{Ops: f})
	res, err := h.Execute(context.Background(), moduleSetJob(perNodeQueue, map[string]any{
		"desired_status": "running",
	}), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure (terminal) for missing source", res.Status)
	}
}

func TestModuleSetListFailureRetries(t *testing.T) {
	f := newFakeModuleOps()
	f.failOps["list"] = fmt.Errorf("docker down")
	h := NewModuleSetHandler(ModuleSetConfig{Ops: f})
	res, err := h.Execute(context.Background(), moduleSetJob(perNodeQueue, map[string]any{
		"source": "owner/repo@v1", "desired_status": "running",
	}), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusRetry {
		t.Fatalf("status = %v, want retry on transient list failure", res.Status)
	}
}

func TestModuleSetInstallFailureRetries(t *testing.T) {
	f := newFakeModuleOps()
	f.failOps["install"] = fmt.Errorf("clone failed")
	h := NewModuleSetHandler(ModuleSetConfig{Ops: f})
	res, err := h.Execute(context.Background(), moduleSetJob(perNodeQueue, map[string]any{
		"source": "owner/repo@v1", "desired_status": "running",
	}), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusRetry {
		t.Fatalf("status = %v, want retry on transient install failure", res.Status)
	}
}
