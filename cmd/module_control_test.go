// cmd/module_control_test.go
package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	err := runModuleControl("does-not-exist", moduleActionStop)
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

	err := runModuleControl("vllm", moduleActionStop)
	if err == nil {
		t.Fatal("want an error when no node manifest exists")
	}
}

func TestRunModuleControlNotComposeManaged(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: "ollama-native", Type: "native", Port: 11434}, // no ComposeFile
	})

	err := runModuleControl("ollama-native", moduleActionStop)
	if err == nil {
		t.Fatal("want an error for a service with no compose_file")
	}
	if !strings.Contains(err.Error(), "not a docker-compose-managed service") {
		t.Fatalf("error = %v, want it to explain the service is not compose-managed", err)
	}
}
