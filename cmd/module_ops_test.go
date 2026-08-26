// cmd/module_ops_test.go
package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/reconcile"
	"gopkg.in/yaml.v3"
)

// writeLockfile writes a modules.lock into the isolated HOME's config dir
// (platform.ConfigDir() == $HOME/.citadel-cli for a non-root test process,
// matching writeManifestWithServices's configDir). Callers must have already
// set HOME (e.g. via writeManifestWithServices) before calling this.
func writeLockfile(t *testing.T, entries []catalog.LockEntry) {
	t.Helper()

	home := os.Getenv("HOME")
	if home == "" {
		t.Fatal("writeLockfile: HOME not set; call writeManifestWithServices first")
	}
	configDir := filepath.Join(home, ".citadel-cli")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	lf := catalog.Lockfile{Version: 1, Modules: entries}
	data, err := yaml.Marshal(lf)
	if err != nil {
		t.Fatalf("marshal lockfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "modules.lock"), data, 0o600); err != nil {
		t.Fatalf("write lockfile: %v", err)
	}
}

// newTestModuleOps builds a liveModuleOps with docker/compose side effects
// stubbed out, so ListInstalled's pure enumeration/health logic can be tested
// without a container runtime.
func newTestModuleOps(running map[string]bool) *liveModuleOps {
	return &liveModuleOps{
		log:       func(string, ...any) {},
		isRunning: func(name string) bool { return running[name] },
	}
}

// elevenManifestServices returns 11 manifest services: exactly one
// ("module-a") is module-managed (has a lockfile entry in the tests below);
// the other 10 are operator-run / embedded services that were never installed
// through the module system (the citadel#739 scenario).
func elevenManifestServices() []Service {
	services := []Service{{Name: "module-a", ComposeFile: filepath.Join("services", "module-a.yml")}}
	for i := 1; i <= 10; i++ {
		services = append(services, Service{
			Name:        "embedded-" + string(rune('a'+i-1)),
			ComposeFile: filepath.Join("services", "embedded.yml"),
		})
	}
	return services
}

// TestListInstalledScopedToLockfile is the direct regression test for
// citadel#739: ListInstalled must enumerate ONLY lockfile (module-system)
// entries, not every manifest service. An 11-service manifest with a single
// lockfile entry must report exactly that one module as "installed".
func TestListInstalledScopedToLockfile(t *testing.T) {
	writeManifestWithServices(t, elevenManifestServices())
	writeLockfile(t, []catalog.LockEntry{
		{Name: "module-a", Source: "owner/module-a@v1.0.0", Ref: "v1.0.0"},
	})

	o := newTestModuleOps(map[string]bool{"module-a": true})
	got, err := o.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("ListInstalled: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 installed module (lockfile-scoped), got %d: %+v", len(got), got)
	}
	if got[0].Name != "module-a" || got[0].Source != "owner/module-a@v1.0.0" {
		t.Errorf("unexpected module: %+v", got[0])
	}
	if got[0].Health != reconcile.HealthRunning {
		t.Errorf("health = %q, want running", got[0].Health)
	}
}

// TestListInstalledEmptyLockfileReportsNothing confirms that a manifest full
// of operator-run/embedded services with NO lockfile at all reports zero
// installed modules -- i.e. the reconcile engine's authority starts empty on
// a node that has never used the module system, regardless of how many
// services citadel.yaml lists.
func TestListInstalledEmptyLockfileReportsNothing(t *testing.T) {
	writeManifestWithServices(t, elevenManifestServices())
	// No lockfile written at all: catalog.LoadLockfile returns an empty
	// (version 1) lockfile when the file does not exist.

	o := newTestModuleOps(map[string]bool{})
	got, err := o.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("ListInstalled: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 installed modules on a lockfile-less node, got %d: %+v", len(got), got)
	}
}

// TestOneDesiredModuleDoesNotWipeManifest is the end-to-end regression for
// citadel#739: a real reconcile.Reconcile() diff, fed the ACTUAL scoping this
// fix produces, against a desired state naming only the one module-managed
// service, must not plan to uninstall the other 10 manifest services.
//
// Before the fix, ListInstalled reported all 11 manifest services as
// "installed", so this exact scenario produced 10 ActionUninstall steps.
func TestOneDesiredModuleDoesNotWipeManifest(t *testing.T) {
	writeManifestWithServices(t, elevenManifestServices())
	writeLockfile(t, []catalog.LockEntry{
		{Name: "module-a", Source: "owner/module-a@v1.0.0", Ref: "v1.0.0"},
	})

	o := newTestModuleOps(map[string]bool{"module-a": true})
	actual, err := o.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("ListInstalled: %v", err)
	}

	desired := reconcile.DesiredState{
		Revision: "rev-1",
		Modules: []reconcile.ModuleAssignment{
			{Name: "module-a", Source: "owner/module-a@v1.0.0"},
		},
	}

	plan, err := reconcile.Reconcile(context.Background(), desired, actual)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !plan.IsEmpty() {
		t.Fatalf("want an empty (already-converged) plan, got %d steps: %+v", len(plan.Steps), plan.Steps)
	}
	for _, step := range plan.Steps {
		if step.Action == reconcile.ActionUninstall {
			t.Fatalf("one desired module must not trigger uninstalling manifest service %q (citadel#739)", step.Name)
		}
	}
}

// TestDesiredNamingUnmanagedEmbeddedServiceReinstallsRatherThanAdopts pins a
// documented TRADE-OFF of the #739 fix (see PR discussion): before the fix, a
// desired assignment whose Source matched an embedded/operator-run manifest
// service's bare name (e.g. "vllm") diffed as already-installed-and-converged
// -- a silent, provenance-free "adopt" of a container the module system never
// installed. After the fix that service is invisible to ListInstalled (no
// lockfile entry), so the SAME assignment now plans an install: the engine
// no longer pretends a manifest service is module-managed just because the
// names line up. This is the accepted cost of scoping reconcile's authority
// to the lockfile -- a real service (re)install/compose churn the FIRST time
// the control plane targets an already-embedded, not-yet-module-managed
// service by name, in exchange for the module system gaining real
// provenance over it going forward, and never uninstalling it by surprise.
func TestDesiredNamingUnmanagedEmbeddedServiceReinstallsRatherThanAdopts(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: "vllm", ComposeFile: filepath.Join("services", "vllm.yml")},
	})
	// No lockfile: "vllm" here is an embedded/operator-run service, never
	// installed through the module system.

	o := newTestModuleOps(map[string]bool{"vllm": true})
	actual, err := o.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("ListInstalled: %v", err)
	}
	if len(actual) != 0 {
		t.Fatalf("want the unmanaged embedded service invisible to ListInstalled, got %+v", actual)
	}

	desired := reconcile.DesiredState{
		Revision: "rev-1",
		Modules:  []reconcile.ModuleAssignment{{Name: "vllm", Source: "vllm"}},
	}
	plan, err := reconcile.Reconcile(context.Background(), desired, actual)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Action != reconcile.ActionInstall || plan.Steps[0].Name != "vllm" {
		t.Fatalf("want a single install step for vllm (documented trade-off), got %+v", plan.Steps)
	}
}

// TestGotenbergManifestOnlyEntryNeverFalseConverges is the direct regression
// test for citadel#645 ("MODULE_SET converges 'gotenberg' in 0 steps but the
// module never runs").
//
// Root-cause finding: on v2.96.0 (the reported version), liveModuleOps.
// ListInstalled enumerated the MANIFEST `services:` list (see the pre-#739
// implementation, git show v2.96.0:cmd/module_ops.go), so a "gotenberg" entry
// that reached citadel.yaml -- e.g. via a first Install() attempt that ran
// addServiceToManifestWithTags before the compose-up step, then failed or
// left a container in a broken-but-"Running" state -- was reported as
// "already installed" with NO lockfile row required. That is exactly
// reproducible from the issue's own second symptom ("the node does not
// report node_module_state ... 'no module state reported'"): per the #733
// CLAUDE.md note, that platform-visible state is driven by the LOCKFILE being
// empty, which is consistent with a manifest-only, lockfile-less "gotenberg"
// entry -- precisely the case ListInstalled used to (wrongly) report as
// installed.
//
// citadel#739 (merged after v2.96.0, cmd/module_ops.go's ListInstalled)
// re-scoped enumeration to the LOCKFILE, so a manifest-only "gotenberg" entry
// is now invisible to it, and reconcile.Reconcile (whose diff algorithm is
// otherwise unchanged -- see internal/reconcile/engine.go) can no longer
// treat it as already-converged: MODULE_SET now always plans >=1 step
// (install) for it. Confirmed independently that "gotenberg" IS a resolvable
// catalog module (aceteam-ai/citadel-services `services/gotenberg/
// service.yaml` + `compose.yml` exist), so the "unresolvable source" theory
// from the issue does not apply -- this was a state-detection false positive
// in the reconcile engine's actual-state enumeration, now closed.
func TestGotenbergManifestOnlyEntryNeverFalseConverges(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: "gotenberg", ComposeFile: filepath.Join("services", "gotenberg.yml")},
	})
	// No lockfile: models the exact v2.96.0 symptom ("no module state
	// reported") -- a "gotenberg" manifest entry with no module-system
	// provenance recorded.

	// The container LOOKS running (e.g. a crash-looping "Running: true"
	// sample, or a container left over from a broken earlier attempt) --
	// even so, an entry with no lockfile row must be invisible.
	o := newTestModuleOps(map[string]bool{"gotenberg": true})
	actual, err := o.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("ListInstalled: %v", err)
	}
	if len(actual) != 0 {
		t.Fatalf("want the lockfile-less gotenberg entry invisible to ListInstalled (citadel#645), got %+v", actual)
	}

	desired := reconcile.DesiredState{
		Revision: "rev-1",
		Modules:  []reconcile.ModuleAssignment{{Name: "gotenberg", Source: "gotenberg", DesiredStatus: reconcile.StatusRunning}},
	}
	plan, err := reconcile.Reconcile(context.Background(), desired, actual)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if plan.IsEmpty() {
		t.Fatalf("citadel#645: MODULE_SET must never report a 0-step false converge for an uninstalled gotenberg")
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Action != reconcile.ActionInstall || plan.Steps[0].Name != "gotenberg" {
		t.Fatalf("want a single install step for gotenberg, got %+v", plan.Steps)
	}
}

// TestListInstalledNoManifestReportsNothing pins the preserved (pre-existing,
// unrelated to #739) short-circuit: no manifest means nothing is reported as
// installed, even if a lockfile exists -- a node with no manifest has no
// compose files to run anything from regardless of what the lockfile claims.
func TestListInstalledNoManifestReportsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeLockfile(t, []catalog.LockEntry{{Name: "module-a", Source: "owner/module-a"}})
	// No global config / manifest written at all.

	o := newTestModuleOps(map[string]bool{"module-a": true})
	got, err := o.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("ListInstalled: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 installed modules without a readable manifest, got %d: %+v", len(got), got)
	}
}

// TestListInstalledHonorsStoppedMarker confirms the durable stopped marker
// (Service.DesiredStatus == "stopped") still wins over a live "running"
// container check for module-managed services -- unchanged behavior, now
// scoped through the lockfile enumeration.
func TestListInstalledHonorsStoppedMarker(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: "module-a", ComposeFile: filepath.Join("services", "module-a.yml"), DesiredStatus: "stopped"},
	})
	writeLockfile(t, []catalog.LockEntry{{Name: "module-a", Source: "owner/module-a"}})

	// Even though the container LOOKS running, the durable marker must win.
	o := newTestModuleOps(map[string]bool{"module-a": true})
	got, err := o.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("ListInstalled: %v", err)
	}
	if len(got) != 1 || got[0].Health != reconcile.HealthStopped {
		t.Fatalf("want module-a reported stopped (durable marker wins), got %+v", got)
	}
}
