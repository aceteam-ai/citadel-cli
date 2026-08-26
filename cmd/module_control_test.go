// cmd/module_control_test.go
package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// newControlTestOps builds a liveModuleOps with docker/compose side effects
// stubbed out and instrumented, so dispatchModuleAction's stop/start/restart
// behavior can be tested without a container runtime. running is mutated by
// the stubs to simulate the real container state transitions liveModuleOps
// drives (compose down -> not running, compose up -> running), and calls
// records every invocation (in order) so a test can assert exactly which
// module's compose stack was touched -- the SCOPED-ness this command exists
// to guarantee.
func newControlTestOps(running map[string]bool) (*liveModuleOps, *[]string) {
	calls := &[]string{}
	return &liveModuleOps{
		log: func(string, ...any) {},
		startFn: func(name, composePath string) error {
			*calls = append(*calls, "start:"+name)
			running[name] = true
			return nil
		},
		composeDown: func(composePath string, remove bool) error {
			// composePathFor resolves "services/<name>.yml" (the convention
			// every test manifest below uses), so recover the name from the
			// path to update the simulated running state and record the call
			// scoped by module name.
			name := strings.TrimSuffix(filepath.Base(composePath), ".yml")
			*calls = append(*calls, "down:"+name)
			running[name] = false
			return nil
		},
		isRunning: func(name string) bool { return running[name] },
	}, calls
}

// twoModuleManifest returns a 2-service manifest (vllm, bonsai), both
// docker-compose-managed, for exercising the "only the named module is
// touched" contract.
func twoModuleManifest() []Service {
	return []Service{
		{Name: "vllm", ComposeFile: filepath.Join("services", "vllm.yml")},
		{Name: "bonsai", ComposeFile: filepath.Join("services", "bonsai.yml")},
	}
}

// writeManifestWithComposeFiles is writeManifestWithServices plus an empty
// on-disk compose file for each service that declares one. liveModuleOps.Stop
// (cmd/module_ops.go) os.Stats the compose path before calling composeDown
// and treats a missing file as "already gone" (a legitimate no-op for a
// module whose stack was already torn down out-of-band) -- so a scoped-stop
// test needs the file to actually exist to exercise the real compose-down
// call, matching how `citadel module install` / catalog install always
// materialize the file before it is ever referenced from the manifest.
func writeManifestWithComposeFiles(t *testing.T, services []Service) string {
	t.Helper()
	configDir := writeManifestWithServices(t, services)
	for _, s := range services {
		if s.ComposeFile == "" {
			continue
		}
		path := filepath.Join(configDir, s.ComposeFile)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("services: {}\n"), 0o644); err != nil {
			t.Fatalf("write compose file %s: %v", path, err)
		}
	}
	return configDir
}

func TestModuleControlStopSetsDesiredStatusAndStopsScoped(t *testing.T) {
	writeManifestWithComposeFiles(t, twoModuleManifest())
	running := map[string]bool{"vllm": true, "bonsai": true}
	ops, calls := newControlTestOps(running)

	if err := dispatchModuleAction(ops, "vllm", moduleActionStop); err != nil {
		t.Fatalf("dispatchModuleAction(stop): %v", err)
	}

	if got := *calls; len(got) != 1 || got[0] != "down:vllm" {
		t.Fatalf("want exactly one scoped stop of vllm, got %v", got)
	}
	if running["bonsai"] != true {
		t.Fatalf("sibling module bonsai must be untouched by a scoped stop, running=%v", running["bonsai"])
	}
	if running["vllm"] != false {
		t.Fatalf("vllm should now be stopped")
	}

	manifest, _, err := findAndReadManifest()
	if err != nil {
		t.Fatalf("findAndReadManifest: %v", err)
	}
	svc := lookupService(manifest, "vllm")
	if !serviceStartDisabled(svc) {
		t.Fatalf("want desired_status:stopped durably recorded for vllm, got DesiredStatus=%q", svc.DesiredStatus)
	}
	bonsai := lookupService(manifest, "bonsai")
	if serviceStartDisabled(bonsai) {
		t.Fatalf("sibling module bonsai must not have its desired_status touched")
	}
}

func TestModuleControlStartClearsDesiredStatusAndStartsScoped(t *testing.T) {
	services := twoModuleManifest()
	services[0].DesiredStatus = "stopped" // vllm starts durably stopped
	writeManifestWithComposeFiles(t, services)
	running := map[string]bool{"vllm": false, "bonsai": true}
	ops, calls := newControlTestOps(running)

	if err := dispatchModuleAction(ops, "vllm", moduleActionStart); err != nil {
		t.Fatalf("dispatchModuleAction(start): %v", err)
	}

	if got := *calls; len(got) != 1 || got[0] != "start:vllm" {
		t.Fatalf("want exactly one scoped start of vllm, got %v", got)
	}
	if running["vllm"] != true {
		t.Fatalf("vllm should now be running")
	}

	manifest, _, err := findAndReadManifest()
	if err != nil {
		t.Fatalf("findAndReadManifest: %v", err)
	}
	svc := lookupService(manifest, "vllm")
	if serviceStartDisabled(svc) {
		t.Fatalf("want desired_status cleared for vllm, got DesiredStatus=%q", svc.DesiredStatus)
	}
}

func TestModuleControlRestartStopsThenStarts(t *testing.T) {
	writeManifestWithComposeFiles(t, twoModuleManifest())
	running := map[string]bool{"vllm": true, "bonsai": true}
	ops, calls := newControlTestOps(running)

	if err := dispatchModuleAction(ops, "vllm", moduleActionRestart); err != nil {
		t.Fatalf("dispatchModuleAction(restart): %v", err)
	}

	want := []string{"down:vllm", "start:vllm"}
	got := *calls
	if len(got) != len(want) {
		t.Fatalf("restart calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restart calls = %v, want %v", got, want)
		}
	}
	if running["vllm"] != true {
		t.Fatalf("vllm should be running again after restart")
	}
	if running["bonsai"] != true {
		t.Fatalf("sibling module bonsai must be untouched by a scoped restart")
	}
}

// TestModuleControlStopAlreadyStoppedStillCallsScopedStop pins the
// correctness fix over a naive "isRunning ? skip : do it" short-circuit: the
// isRunning read is a best-effort, fail-closed `docker inspect` (wrong on a
// docker-unreachable node, or a third-party module whose compose names its
// container differently) and MUST NOT gate whether the real stop actually
// runs. An "already stopped" reading only changes the message; the scoped
// stop is always issued (compose down on an already-down stack is itself a
// safe no-op), so a stale/wrong reading can never leave an engine running
// while this command reports success.
func TestModuleControlStopAlreadyStoppedStillCallsScopedStop(t *testing.T) {
	writeManifestWithComposeFiles(t, twoModuleManifest())
	running := map[string]bool{"vllm": false, "bonsai": true}
	ops, calls := newControlTestOps(running)

	if err := dispatchModuleAction(ops, "vllm", moduleActionStop); err != nil {
		t.Fatalf("dispatchModuleAction(stop): %v", err)
	}

	if got := *calls; len(got) != 1 || got[0] != "down:vllm" {
		t.Fatalf("an 'already stopped' reading must still issue the scoped stop, got calls=%v", got)
	}
	if running["bonsai"] != true {
		t.Fatalf("sibling module bonsai must be untouched, running=%v", running["bonsai"])
	}

	manifest, _, err := findAndReadManifest()
	if err != nil {
		t.Fatalf("findAndReadManifest: %v", err)
	}
	if !serviceStartDisabled(lookupService(manifest, "vllm")) {
		t.Fatalf("want desired_status:stopped recorded on the already-stopped no-op path")
	}
}

// TestModuleControlStartAlreadyRunningStillCallsScopedStart is the start-side
// mirror of TestModuleControlStopAlreadyStoppedStillCallsScopedStop.
func TestModuleControlStartAlreadyRunningStillCallsScopedStart(t *testing.T) {
	writeManifestWithComposeFiles(t, twoModuleManifest())
	running := map[string]bool{"vllm": true, "bonsai": true}
	ops, calls := newControlTestOps(running)

	if err := dispatchModuleAction(ops, "vllm", moduleActionStart); err != nil {
		t.Fatalf("dispatchModuleAction(start): %v", err)
	}

	if got := *calls; len(got) != 1 || got[0] != "start:vllm" {
		t.Fatalf("an 'already running' reading must still issue the scoped start, got calls=%v", got)
	}
}

func TestRunModuleControlUnknownName(t *testing.T) {
	writeManifestWithServices(t, twoModuleManifest())

	err := runModuleControl(context.Background(), "does-not-exist", moduleActionStop)
	if err == nil {
		t.Fatal("want an error for an unknown module name")
	}
	if !strings.Contains(err.Error(), "unknown module") {
		t.Fatalf("error = %v, want it to mention 'unknown module'", err)
	}
	if !strings.Contains(err.Error(), "vllm") || !strings.Contains(err.Error(), "bonsai") {
		t.Fatalf("error = %v, want it to list the installed modules", err)
	}
}

func TestRunModuleControlNoManifest(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolated HOME with no global config / manifest

	err := runModuleControl(context.Background(), "vllm", moduleActionStop)
	if err == nil {
		t.Fatal("want an error when no node manifest exists")
	}
}

func TestRunModuleControlNotComposeManaged(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: "ollama-native", Type: "native", Port: 11434}, // no ComposeFile
	})

	err := runModuleControl(context.Background(), "ollama-native", moduleActionStop)
	if err == nil {
		t.Fatal("want an error for a service with no compose_file")
	}
	if !strings.Contains(err.Error(), "not a docker-compose-managed service") {
		t.Fatalf("error = %v, want it to explain the service is not compose-managed", err)
	}
}

// TestRunModuleControlEmbeddedNotYetInManifest pins the #854 finding: a known
// embedded engine (real ServiceMap entry) that has never been started on this
// node is NOT in the manifest, so module control -- which only ever touches
// services already tracked there -- correctly refuses, but must say so
// precisely (pointing at 'citadel run <name>') rather than folding it into the
// generic "unknown module" message, which would wrongly suggest the name is
// not a real service at all.
func TestRunModuleControlEmbeddedNotYetInManifest(t *testing.T) {
	writeManifestWithServices(t, []Service{}) // no services installed at all

	if _, ok := services.ServiceMap["vllm"]; !ok {
		t.Fatal("test assumption broken: 'vllm' is no longer an embedded ServiceMap entry")
	}

	err := runModuleControl(context.Background(), "vllm", moduleActionStop)
	if err == nil {
		t.Fatal("want an error: vllm is not yet in this node's manifest")
	}
	if strings.Contains(err.Error(), "unknown module") {
		t.Fatalf("error = %v, must NOT say 'unknown module' for a real embedded engine", err)
	}
	if !strings.Contains(err.Error(), "citadel run vllm") {
		t.Fatalf("error = %v, want it to point at 'citadel run vllm'", err)
	}
}

// TestModuleControlWorksForEmbeddedServiceAlreadyInManifest confirms #854
// item 1 directly: 'citadel module start/stop/restart' already works for an
// EMBEDDED engine (not just a catalog-installed module) once that engine is
// tracked in the manifest -- exactly the recovery-path case (a crashed vllm
// that citadel itself already knows about). "vllm" is a real
// services.ServiceMap entry, not a coincidental test name.
func TestModuleControlWorksForEmbeddedServiceAlreadyInManifest(t *testing.T) {
	if _, ok := services.ServiceMap["vllm"]; !ok {
		t.Fatal("test assumption broken: 'vllm' is no longer an embedded ServiceMap entry")
	}
	writeManifestWithComposeFiles(t, []Service{
		{Name: "vllm", ComposeFile: filepath.Join("services", "vllm.yml")},
	})
	running := map[string]bool{"vllm": true}
	ops, calls := newControlTestOps(running)

	if err := dispatchModuleAction(ops, "vllm", moduleActionStop); err != nil {
		t.Fatalf("dispatchModuleAction(stop) on embedded service: %v", err)
	}
	if got := *calls; len(got) != 1 || got[0] != "down:vllm" {
		t.Fatalf("want exactly one scoped stop of the embedded vllm engine, got %v", got)
	}
}

// --- --dry-run (citadel#853) ---

func TestModuleDryRunPlan_ContentsAndFallbackContainerName(t *testing.T) {
	plan := moduleDryRunPlan("vllm", moduleActionStop, "/some/node/dir", "/some/node/dir/services/vllm.yml")
	for _, want := range []string{
		"DRY RUN", "stop module 'vllm'",
		"Resolved node dir: /some/node/dir",
		"Compose file:      /some/node/dir/services/vllm.yml",
		"Container(s):      citadel-vllm", // fallback: compose file doesn't exist on disk here
		"No changes made.",
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("plan = %q, want it to contain %q", plan, want)
		}
	}
}

func TestModuleDryRunPlan_RestartVerb(t *testing.T) {
	plan := moduleDryRunPlan("vllm", moduleActionRestart, "/d", "/d/services/vllm.yml")
	if !strings.Contains(plan, "stop then start (restart) module 'vllm'") {
		t.Fatalf("plan = %q, want the restart verb spelled out", plan)
	}
}

func TestDryRunContainerNames_ReadsExplicitContainerName(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "vllm.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  vllm:\n    container_name: citadel-vllm\n"), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	got := dryRunContainerNames(composePath, "vllm")
	if len(got) != 1 || got[0] != "citadel-vllm" {
		t.Fatalf("dryRunContainerNames = %v, want [citadel-vllm]", got)
	}
}

func TestDryRunContainerNames_FallsBackWhenUnreadable(t *testing.T) {
	got := dryRunContainerNames(filepath.Join(t.TempDir(), "does-not-exist.yml"), "bonsai")
	if len(got) != 1 || got[0] != "citadel-bonsai" {
		t.Fatalf("dryRunContainerNames = %v, want the citadel-<name> fallback", got)
	}
}

// TestRunModuleControl_DryRunDoesNotMutate is the #853 acceptance test for
// --dry-run: it must print a plan and touch NOTHING -- not the manifest's
// desired_status marker, and (by construction, since the dry-run branch
// returns before newLiveModuleOps is ever called) never a real docker
// command either.
func TestRunModuleControl_DryRunDoesNotMutate(t *testing.T) {
	writeManifestWithComposeFiles(t, twoModuleManifest())
	moduleDryRun = true
	t.Cleanup(func() { moduleDryRun = false })

	if err := runModuleControl(context.Background(), "vllm", moduleActionStop); err != nil {
		t.Fatalf("runModuleControl with --dry-run: %v", err)
	}

	manifest, _, err := findAndReadManifest()
	if err != nil {
		t.Fatalf("findAndReadManifest: %v", err)
	}
	if serviceStartDisabled(lookupService(manifest, "vllm")) {
		t.Fatalf("--dry-run must not mutate the manifest's desired_status marker")
	}
}

// --- --expect-node (citadel#853, reuses #844's whoami identity) ---

func TestExpectNodeMatchesFast(t *testing.T) {
	manifest := &CitadelManifest{}
	manifest.Node.Name = "gpu-node-1"

	if !expectNodeMatchesFast(manifest, "gpu-node-1") {
		t.Error("want a match on manifest node name")
	}
	if !expectNodeMatchesFast(manifest, "GPU-NODE-1") {
		t.Error("want a case-insensitive match on manifest node name")
	}
	if expectNodeMatchesFast(manifest, "some-other-node") {
		t.Error("want no match for an unrelated name")
	}

	hostname, err := os.Hostname()
	if err == nil && hostname != "" {
		if !expectNodeMatchesFast(manifest, hostname) {
			t.Errorf("want a match on OS hostname %q", hostname)
		}
	}

	// A numeric mesh ID is NOT resolvable by the fast path -- it requires the
	// live gatherIdentity fallback.
	if expectNodeMatchesFast(manifest, "1084") {
		t.Error("want no fast match for a mesh ID (that requires the live gatherIdentity fallback)")
	}
}

func TestNodeIdentityMatches(t *testing.T) {
	id := NodeIdentity{NodeName: "gpu-node-1", Hostname: "box42", HeadscaleNodeID: "1084"}
	cases := []struct {
		expect string
		want   bool
	}{
		{"", true},
		{"gpu-node-1", true},
		{"GPU-NODE-1", true}, // case-insensitive
		{"box42", true},
		{"1084", true},
		{"some-other-node", false},
	}
	for _, c := range cases {
		if got := nodeIdentityMatches(id, c.expect); got != c.want {
			t.Errorf("nodeIdentityMatches(%+v, %q) = %v, want %v", id, c.expect, got, c.want)
		}
	}
}

// TestRunModuleControl_ExpectNodeMismatchRefuses pins the fail-CLOSED
// contract: a mismatched --expect-node must refuse BEFORE anything happens,
// including a --dry-run preview (a preview a real run would refuse is the
// wrong direction of error) -- and must not touch the manifest.
func TestRunModuleControl_ExpectNodeMismatchRefuses(t *testing.T) {
	configDir := writeManifestWithComposeFiles(t, twoModuleManifest())
	// Give the manifest a distinguishing node name so the mismatch is real.
	manifest, _, err := findAndReadManifest()
	if err != nil {
		t.Fatalf("findAndReadManifest: %v", err)
	}
	manifest.Node.Name = "the-real-production-node"
	if err := writeManifest(filepath.Join(configDir, "citadel.yaml"), manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	moduleExpectNode = "some-isolated-test-node"
	moduleDryRun = true // even in preview mode, a mismatch must still refuse
	t.Cleanup(func() { moduleExpectNode = ""; moduleDryRun = false })

	err = runModuleControl(context.Background(), "vllm", moduleActionStop)
	if err == nil {
		t.Fatal("want a refusal error on --expect-node mismatch")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("error = %v, want it to say it is refusing", err)
	}

	manifestAfter, _, err := findAndReadManifest()
	if err != nil {
		t.Fatalf("findAndReadManifest: %v", err)
	}
	if serviceStartDisabled(lookupService(manifestAfter, "vllm")) {
		t.Fatalf("a refused --expect-node mismatch must not mutate the manifest")
	}
}

// TestRunModuleControl_ExpectNodeMatchProceeds is the match-side mirror:
// a correct --expect-node must NOT refuse. Combined with --dry-run so the
// test stays hermetic (no real docker/compose dependency) while still proving
// the gate itself passes and execution continues past it.
func TestRunModuleControl_ExpectNodeMatchProceeds(t *testing.T) {
	configDir := writeManifestWithComposeFiles(t, twoModuleManifest())
	manifest, _, err := findAndReadManifest()
	if err != nil {
		t.Fatalf("findAndReadManifest: %v", err)
	}
	manifest.Node.Name = "my-test-node"
	if err := writeManifest(filepath.Join(configDir, "citadel.yaml"), manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	moduleExpectNode = "my-test-node"
	moduleDryRun = true
	t.Cleanup(func() { moduleExpectNode = ""; moduleDryRun = false })

	if err := runModuleControl(context.Background(), "vllm", moduleActionStop); err != nil {
		t.Fatalf("runModuleControl with matching --expect-node: %v", err)
	}
}

// --- --node-dir end to end through module control (citadel#853) ---

// TestNodeDirOverride_ModuleControlTargetsAlternateDirNotHome is the full
// acceptance test: --node-dir points 'citadel module stop' at an alternate
// (temp) directory instead of $HOME, the override dir's manifest is read AND
// WRITTEN (desired_status:stopped recorded), and the real $HOME manifest is
// left completely untouched -- the exact incident this feature exists to
// prevent, reproduced and proven fixed.
func TestNodeDirOverride_ModuleControlTargetsAlternateDirNotHome(t *testing.T) {
	// "Real" $HOME / production node: vllm is running there. Must stay running
	// (desired_status must stay unset) throughout this test.
	writeManifestWithComposeFiles(t, twoModuleManifest())
	prodManifestPath := func() string {
		_, dir, err := findAndReadManifest()
		if err != nil {
			t.Fatalf("resolve real $HOME manifest: %v", err)
		}
		return filepath.Join(dir, "citadel.yaml")
	}()

	// Isolated override dir: its OWN manifest + compose files, distinct from
	// $HOME's.
	overrideDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(overrideDir, "services"), 0o755); err != nil {
		t.Fatalf("mkdir override services dir: %v", err)
	}
	writeYAMLFile(t, filepath.Join(overrideDir, "citadel.yaml"), &CitadelManifest{
		Services: twoModuleManifest(),
	})
	for _, s := range twoModuleManifest() {
		composePath := filepath.Join(overrideDir, s.ComposeFile)
		if err := os.MkdirAll(filepath.Dir(composePath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(composePath, []byte("services: {}\n"), 0o644); err != nil {
			t.Fatalf("write compose file: %v", err)
		}
	}

	setNodeDirOverrideForTest(t, overrideDir)

	running := map[string]bool{"vllm": true, "bonsai": true}
	ops, calls := newControlTestOps(running)
	if err := dispatchModuleAction(ops, "vllm", moduleActionStop); err != nil {
		t.Fatalf("dispatchModuleAction(stop) under --node-dir: %v", err)
	}
	if got := *calls; len(got) != 1 || got[0] != "down:vllm" {
		t.Fatalf("want exactly one scoped stop of vllm in the OVERRIDE dir, got %v", got)
	}

	// The override dir's manifest must show vllm durably stopped.
	overrideManifest, resolvedDir, err := findAndReadManifest()
	if err != nil {
		t.Fatalf("findAndReadManifest (still under override): %v", err)
	}
	if resolvedDir != overrideDir {
		t.Fatalf("resolved dir = %q, want the override dir %q", resolvedDir, overrideDir)
	}
	if !serviceStartDisabled(lookupService(overrideManifest, "vllm")) {
		t.Fatalf("want desired_status:stopped recorded in the OVERRIDE manifest")
	}

	// The real $HOME manifest, read directly off disk (bypassing the override
	// so this assertion doesn't accidentally re-read the override dir), must
	// be completely untouched: vllm still has no desired_status marker.
	prodData, err := os.ReadFile(prodManifestPath)
	if err != nil {
		t.Fatalf("read real $HOME manifest: %v", err)
	}
	if strings.Contains(string(prodData), "desired_status") {
		t.Fatalf("real $HOME manifest must be untouched by an action under --node-dir, got:\n%s", prodData)
	}
}
