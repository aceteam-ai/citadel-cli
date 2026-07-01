package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// writeManifestWithServices creates an isolated HOME containing a global config
// and a node manifest with the given services, so findAndReadManifest resolves
// to the temp tree. It returns the node config dir.
func writeManifestWithServices(t *testing.T, services []Service) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	configDir := filepath.Join(home, ".citadel-cli")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	nodeDir := filepath.Join(home, "citadel-node")
	if err := os.MkdirAll(filepath.Join(nodeDir, "services"), 0o755); err != nil {
		t.Fatalf("mkdir node dir: %v", err)
	}

	globalConfig := map[string]string{"node_config_dir": nodeDir}
	globalData, err := yaml.Marshal(globalConfig)
	if err != nil {
		t.Fatalf("marshal global config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), globalData, 0o600); err != nil {
		t.Fatalf("write global config: %v", err)
	}

	manifest := &CitadelManifest{Services: services}
	manifestData, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "citadel.yaml"), manifestData, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	return nodeDir
}

// TestStartManagedServicesNoManifest verifies that with no manifest present the
// helper returns immediately with nothing started — service startup must never
// block or fail the worker just because no manifest exists.
func TestStartManagedServicesNoManifest(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolated HOME with no global config / manifest

	started := startManagedServices(context.Background())
	if started != nil {
		t.Fatalf("expected no services started without a manifest, got %v", started)
	}
}

// TestStartManagedServicesCanceledContextDoesNotStart is the core sequencing
// contract for issue #384: job-stream subscription must not be gated on service
// readiness. We model shutdown-during-startup with an already-canceled context;
// the helper must observe cancellation and return without ever shelling out to
// docker/podman to start the (slow) service. If cancellation were ignored the
// call would attempt to start "vllm" and either block or error on the missing
// compose file.
func TestStartManagedServicesCanceledContextDoesNotStart(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: "vllm", Type: "docker", ComposeFile: filepath.Join("services", "vllm.yml")},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown already requested before startup begins

	started := startManagedServices(ctx)
	if len(started) != 0 {
		t.Fatalf("expected no services started under a canceled context, got %v", started)
	}
}

// TestStopManagedServicesEmptyIsNoop verifies teardown of an empty started-set
// is a no-op (the double-call path: signal handler + deferred cleanup).
func TestStopManagedServicesEmptyIsNoop(t *testing.T) {
	// Must not panic or touch docker when nothing was started.
	stopManagedServices(nil)
	stopManagedServices([]startedService{})
}

// TestWaitServicesFlagDefaultsToAsync documents the default: services start
// asynchronously (workWaitServices=false) so subscription is not gated on them.
func TestWaitServicesFlagDefaultsToAsync(t *testing.T) {
	if flag := workCmd.Flags().Lookup("wait-services"); flag == nil {
		t.Fatal("expected --wait-services flag to be registered")
	} else if flag.DefValue != "false" {
		t.Errorf("--wait-services default = %q, want \"false\" (async startup is the default)", flag.DefValue)
	}
	if flag := workCmd.Flags().Lookup("no-services"); flag == nil {
		t.Fatal("expected --no-services flag to remain registered")
	}
}
