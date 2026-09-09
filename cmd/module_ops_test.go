// cmd/module_ops_test.go
package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/reconcile"
	"github.com/aceteam-ai/citadel-cli/internal/whatsapp"
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
	// module-a is desired-state-managed (STAMPED) and in the desired set below;
	// legacy-mod is a lockfile-recorded but UNSTAMPED module (operator/catalog
	// CLI-installed) that is NOT in the desired set -- the citadel#624 D1
	// protected case, layered onto the #739 manifest-service case.
	writeLockfile(t, []catalog.LockEntry{
		{Name: "module-a", Source: "owner/module-a@v1.0.0", Ref: "v1.0.0", ManagedBy: reconcile.ManagedByDesiredState},
		{Name: "legacy-mod", Source: "owner/legacy-mod@v1.0.0", Ref: "v1.0.0"},
	})

	o := newTestModuleOps(map[string]bool{"module-a": true, "legacy-mod": true})
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
			t.Fatalf("one desired module must not trigger uninstalling %q (citadel#739 manifest service / citadel#624 D1 unstamped lockfile sibling)", step.Name)
		}
	}
}

// TestBridgeDesiredDoesNotUninstallUnstampedSibling is the citadel#624 D1
// blast-radius regression at the cmd/ListInstalled level: a non-empty desired
// set containing ONLY the module-managed WhatsApp bridge must NOT uninstall a
// lockfile-recorded but UNSTAMPED sibling (an operator/catalog CLI install).
// The stamped bridge and the unstamped sibling both come out of the real
// liveModuleOps.ListInstalled path, then feed reconcile.Reconcile directly.
func TestBridgeDesiredDoesNotUninstallUnstampedSibling(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: whatsapp.ServiceName, ComposeFile: filepath.Join("services", whatsapp.ServiceName+".yml")},
		{Name: "operator-mod", ComposeFile: filepath.Join("services", "operator-mod.yml")},
	})
	writeLockfile(t, []catalog.LockEntry{
		// The bridge, module-managed (STAMPED) with a compose-service health source.
		{Name: whatsapp.ServiceName, Source: whatsapp.ServiceName, ManagedBy: reconcile.ManagedByDesiredState, HealthComposeService: whatsapp.BridgeService},
		// An operator-installed sibling: recorded but UNSTAMPED -> protected.
		{Name: "operator-mod", Source: "owner/operator-mod@v1"},
	})

	o := newTestModuleOps(map[string]bool{"operator-mod": true})
	// The bridge's health resolves via its compose service, not the citadel-<name>
	// convention (#436); report it running so it converges rather than needing a
	// start.
	o.composeServiceRunning = func(project, service string) bool {
		return service == whatsapp.BridgeService
	}
	actual, err := o.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("ListInstalled: %v", err)
	}

	desired := reconcile.DesiredState{
		Revision: "rev-1",
		Modules: []reconcile.ModuleAssignment{
			{Name: whatsapp.ServiceName, Source: whatsapp.ServiceName},
		},
	}
	plan, err := reconcile.Reconcile(context.Background(), desired, actual)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, step := range plan.Steps {
		if step.Action == reconcile.ActionUninstall {
			t.Fatalf("a lone desired bridge must NOT uninstall unstamped sibling %q (citadel#624 D1)", step.Name)
		}
	}
}

// TestBridgeModuleInstalledUsesConvergentManifestNotLockfile pins citadel#624
// FIX A: D5 delegation must key on node-MANIFEST membership (machine-convergent,
// resolved through config.yaml -> node_config_dir, the SAME path the module
// install writes to) rather than the invoker-scoped lockfile. This test writes a
// manifest that REGISTERS the bridge (what a module/catalog install does via
// addServiceToManifest) but writes NO lockfile at all -- proving the signal does
// not depend on the lockfile that diverges between a root systemd `citadel work`
// (/etc/citadel or /root/.citadel-cli) and an interactive user (~/.citadel-cli).
// So a root MODULE_SET install is seen by an interactive `citadel whatsapp
// provision` that shares the node_config_dir, which is the systemd-topology
// agreement FIX A restores (the bespoke deploy previously ran OVER the module
// files because the lockfile signal missed).
func TestBridgeModuleInstalledUsesConvergentManifestNotLockfile(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: whatsapp.ServiceName, ComposeFile: filepath.Join("services", whatsapp.ServiceName+".yml")},
	})
	// Deliberately NO writeLockfile: the manifest alone must drive detection.
	if !bridgeModuleInstalled() {
		t.Fatal("a bridge registered in the manifest (module-installed) must be detected as module-managed, with no lockfile present (citadel#624 FIX A)")
	}
}

// TestBridgeModuleInstalledFalseForBespokeOnly pins the other half of FIX A: the
// bespoke `citadel whatsapp` deploy NEVER adds the bridge to the manifest (see
// runWhatsAppUp), so a node with only a bespoke bridge must NOT delegate --
// otherwise a re-provision would skip the deploy and silently do nothing.
func TestBridgeModuleInstalledFalseForBespokeOnly(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: "vllm", ComposeFile: filepath.Join("services", "vllm.yml")},
	})
	if bridgeModuleInstalled() {
		t.Fatal("a bespoke-only bridge (absent from the manifest) must NOT be treated as module-managed (citadel#624 FIX A)")
	}
}

// TestListInstalledBridgeSteadyStateNoUpdateWithConfig pins citadel#624 FIX F:
// the steady state must converge to an EMPTY plan even when both the desired
// assignment and the lockfile carry a real (non-empty) config -- no perpetual
// ActionUpdate. It exercises reconcile.sameConfig on a populated map (the earlier
// health test used empty config on both sides), guarding against a future change
// that lets carried/secret config drift the two sides apart.
func TestListInstalledBridgeSteadyStateNoUpdateWithConfig(t *testing.T) {
	cfg := map[string]string{"BRIDGE_PORT": "8123", "PUBLIC_URL": "https://x", "LOG_LEVEL": "info"}
	writeManifestWithServices(t, []Service{
		{Name: whatsapp.ServiceName, ComposeFile: filepath.Join("services", whatsapp.ServiceName+".yml")},
	})
	writeLockfile(t, []catalog.LockEntry{
		{Name: whatsapp.ServiceName, Source: whatsapp.ServiceName, ManagedBy: reconcile.ManagedByDesiredState, HealthComposeService: whatsapp.BridgeService, Config: cfg},
	})
	o := newTestModuleOps(map[string]bool{})
	o.composeServiceRunning = func(project, service string) bool { return true }
	actual, err := o.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("ListInstalled: %v", err)
	}
	desired := reconcile.DesiredState{
		Revision: "rev-1",
		Modules: []reconcile.ModuleAssignment{
			{Name: whatsapp.ServiceName, Source: whatsapp.ServiceName, Config: cfg},
		},
	}
	plan, err := reconcile.Reconcile(context.Background(), desired, actual)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !plan.IsEmpty() {
		t.Fatalf("steady state with matching non-empty config must converge EMPTY (no perpetual ActionUpdate), got %v", plan.Steps)
	}
}

// TestListInstalledBridgeHealthViaComposeService pins citadel#624 sub-collision
// 3: a module that declared health_check.compose_service (persisted as
// HealthComposeService) reports its run-state via the compose PROJECT+SERVICE,
// NOT the citadel-<name> container-inspect convention -- whose name never
// matches the bridge's <project>-bridge-N container (#436) and would report
// STOPPED forever, driving a redundant ActionStart every reconcile pass. With
// the compose-service probe reporting RUNNING, the converged plan is EMPTY.
func TestListInstalledBridgeHealthViaComposeService(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: whatsapp.ServiceName, ComposeFile: filepath.Join("services", whatsapp.ServiceName+".yml")},
	})
	writeLockfile(t, []catalog.LockEntry{
		{Name: whatsapp.ServiceName, Source: whatsapp.ServiceName, ManagedBy: reconcile.ManagedByDesiredState, HealthComposeService: whatsapp.BridgeService},
	})

	o := newTestModuleOps(map[string]bool{}) // isRunning would report STOPPED (the wrong container)
	var probedProject, probedService string
	o.composeServiceRunning = func(project, service string) bool {
		probedProject, probedService = project, service
		return true // the bridge IS up under its compose project
	}
	actual, err := o.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("ListInstalled: %v", err)
	}
	if len(actual) != 1 || actual[0].Health != reconcile.HealthRunning {
		t.Fatalf("bridge must report RUNNING via its compose service; got %+v", actual)
	}
	if probedService != whatsapp.BridgeService {
		t.Fatalf("health must probe the declared compose service %q, probed %q", whatsapp.BridgeService, probedService)
	}
	if probedProject == "" {
		t.Fatal("health probe must carry a non-empty compose project")
	}

	desired := reconcile.DesiredState{
		Revision: "rev-1",
		Modules: []reconcile.ModuleAssignment{
			{Name: whatsapp.ServiceName, Source: whatsapp.ServiceName},
		},
	}
	plan, err := reconcile.Reconcile(context.Background(), desired, actual)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !plan.IsEmpty() {
		t.Fatalf("a healthy module-managed bridge must converge to an EMPTY plan (no perpetual ActionStart), got %v", plan.Steps)
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
