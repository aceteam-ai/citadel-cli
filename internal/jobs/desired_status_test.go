package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"gopkg.in/yaml.v3"
)

// testManifestYAML is a realistic citadel.yaml with fields the minimal
// serviceManifest struct does not model (org_id, capabilities, a module
// service). The durable-stop tests assert these survive every rewrite.
const testManifestYAML = `node:
  name: test-node
  tags:
    - gpu
  org_id: org-123
services:
  - name: vllm
    type: docker
    compose_file: ./services/vllm.yml
  - name: my-module
    compose_file: ./services/my-module.yml
    desired_status: stopped
  - name: stub-528
    type: docker
    compose_file: ./services/stub-528.yml
capabilities:
  engines:
    - vllm
`

func writeTestManifest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "citadel.yaml"), []byte(testManifestYAML), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

// readManifestMap parses the manifest into a generic map for assertions on
// fields the typed structs do not model.
func readManifestMap(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "citadel.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return m
}

// manifestServiceEntry returns the service entry with the given name, or nil.
func manifestServiceEntry(t *testing.T, m map[string]any, name string) map[string]any {
	t.Helper()
	services, _ := m["services"].([]any)
	for _, raw := range services {
		entry, _ := raw.(map[string]any)
		if entry != nil && entry["name"] == name {
			return entry
		}
	}
	return nil
}

func TestSetDesiredStatusInManifestFile(t *testing.T) {
	dir := writeTestManifest(t)
	h := NewServiceHandler(dir)

	t.Run("set stopped", func(t *testing.T) {
		if err := h.setDesiredStatusInManifestFile("vllm", "stopped"); err != nil {
			t.Fatalf("set: %v", err)
		}
		m := readManifestMap(t, dir)
		entry := manifestServiceEntry(t, m, "vllm")
		if entry == nil {
			t.Fatal("vllm entry missing after rewrite")
		}
		if entry["desired_status"] != "stopped" {
			t.Errorf("desired_status = %v, want stopped", entry["desired_status"])
		}
		// The rewrite must not disturb fields outside the target entry.
		if entry["type"] != "docker" {
			t.Errorf("type = %v, want docker (dropped by rewrite?)", entry["type"])
		}
		node, _ := m["node"].(map[string]any)
		if node == nil || node["org_id"] != "org-123" {
			t.Errorf("node.org_id lost in rewrite: %v", m["node"])
		}
		if m["capabilities"] == nil {
			t.Error("capabilities section lost in rewrite")
		}
		if other := manifestServiceEntry(t, m, "my-module"); other == nil || other["desired_status"] != "stopped" {
			t.Errorf("unrelated service my-module disturbed: %v", other)
		}
	})

	t.Run("clear", func(t *testing.T) {
		if err := h.setDesiredStatusInManifestFile("vllm", ""); err != nil {
			t.Fatalf("clear: %v", err)
		}
		entry := manifestServiceEntry(t, readManifestMap(t, dir), "vllm")
		if entry == nil {
			t.Fatal("vllm entry missing after clear")
		}
		if _, present := entry["desired_status"]; present {
			t.Errorf("desired_status still present after clear: %v", entry["desired_status"])
		}
	})

	t.Run("unknown service errors", func(t *testing.T) {
		if err := h.setDesiredStatusInManifestFile("nope", "stopped"); err == nil {
			t.Error("expected error for unknown service, got nil")
		}
	})
}

// TestServiceStopJobSetsDurableMarker verifies the operator-intent contract of
// #528: a remote SERVICE_STOP marks the service durably stopped in citadel.yaml
// (so `citadel work`/`citadel run` will not resurrect it), and a SERVICE_START
// clears the marker again. Uses the synthetic service "stub-528" (never a real
// engine name) so the test cannot touch actual containers on a dev machine;
// without a matching container the compose calls fail or short-circuit, but
// the marker write happens first.
func TestServiceStopJobSetsDurableMarker(t *testing.T) {
	dir := writeTestManifest(t)
	h := NewServiceHandler(dir)
	ctx := JobContext{LogFn: func(string, string) {}}

	stop := &nexus.Job{ID: "j1", Type: "SERVICE_STOP", Payload: map[string]string{"service": "stub-528"}}
	if _, err := h.Execute(ctx, stop); err != nil {
		t.Fatalf("SERVICE_STOP execute: %v", err)
	}
	entry := manifestServiceEntry(t, readManifestMap(t, dir), "stub-528")
	if entry == nil || entry["desired_status"] != "stopped" {
		t.Fatalf("SERVICE_STOP did not set durable marker: %v", entry)
	}

	start := &nexus.Job{ID: "j2", Type: "SERVICE_START", Payload: map[string]string{"service": "stub-528"}}
	// The start itself may fail (no docker / no compose file in this test env);
	// the marker must be cleared regardless, recording the operator's intent.
	_, _ = h.Execute(ctx, start)
	entry = manifestServiceEntry(t, readManifestMap(t, dir), "stub-528")
	if entry == nil {
		t.Fatal("stub-528 entry missing after SERVICE_START")
	}
	if _, present := entry["desired_status"]; present {
		t.Errorf("SERVICE_START did not clear durable marker: %v", entry["desired_status"])
	}
}

// TestStopServiceByNameDoesNotSetMarker pins the autostop contract: the
// auto-stop-when-idle reconciler (#416) evicts ACTUAL state only. It must not
// write desired_status:stopped, or an idle-evicted engine would never come
// back on the next boot.
func TestStopServiceByNameDoesNotSetMarker(t *testing.T) {
	dir := writeTestManifest(t)
	h := NewServiceHandler(dir)

	_ = h.StopServiceByName("stub-528") // may error without docker; irrelevant here
	entry := manifestServiceEntry(t, readManifestMap(t, dir), "stub-528")
	if entry == nil {
		t.Fatal("stub-528 entry missing")
	}
	if _, present := entry["desired_status"]; present {
		t.Errorf("autostop path must not set desired_status, got %v", entry["desired_status"])
	}
}

// TestUpdateManifestMerges verifies APPLY_DEVICE_CONFIG merges the wizard's
// service list instead of rebuilding from nil (#528): module-installed
// services and durable desired_status markers survive, listed services are
// added, and org_id/capabilities round-trip.
func TestUpdateManifestMerges(t *testing.T) {
	dir := writeTestManifest(t)
	// Mark vllm stopped first so the merge has a marker to preserve.
	if err := NewServiceHandler(dir).setDesiredStatusInManifestFile("vllm", "stopped"); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	h := NewConfigHandler(dir)
	cfg := &DeviceConfig{
		DeviceName: "renamed-node",
		Services:   []string{"vllm", "ollama"},
	}
	if err := h.updateManifest(dir, cfg); err != nil {
		t.Fatalf("updateManifest: %v", err)
	}

	m := readManifestMap(t, dir)

	// Module service not in the wizard config must survive, marker intact.
	if entry := manifestServiceEntry(t, m, "my-module"); entry == nil {
		t.Error("module service my-module was dropped by config apply")
	} else if entry["desired_status"] != "stopped" {
		t.Errorf("my-module desired_status = %v, want stopped", entry["desired_status"])
	}

	// Existing listed service keeps its durable marker and type.
	if entry := manifestServiceEntry(t, m, "vllm"); entry == nil {
		t.Fatal("vllm dropped by config apply")
	} else {
		if entry["desired_status"] != "stopped" {
			t.Errorf("vllm desired_status = %v, want stopped (marker wiped)", entry["desired_status"])
		}
		if entry["type"] != "docker" {
			t.Errorf("vllm type = %v, want docker", entry["type"])
		}
	}

	// Newly listed embedded service is registered and materialized.
	if entry := manifestServiceEntry(t, m, "ollama"); entry == nil {
		t.Error("ollama not added by config apply")
	}
	if _, err := os.Stat(filepath.Join(dir, "services", "ollama.yml")); err != nil {
		t.Errorf("ollama compose file not materialized: %v", err)
	}

	// Fields outside the wizard's knowledge must round-trip.
	node, _ := m["node"].(map[string]any)
	if node == nil || node["org_id"] != "org-123" {
		t.Errorf("node.org_id lost: %v", m["node"])
	}
	if node != nil && node["name"] != "renamed-node" {
		t.Errorf("node.name = %v, want renamed-node", node["name"])
	}
	if m["capabilities"] == nil {
		t.Error("capabilities section lost by config apply")
	}
	caps, _ := m["capabilities"].(map[string]any)
	if caps != nil {
		if engines, _ := caps["engines"].([]any); len(engines) != 1 || engines[0] != "vllm" {
			t.Errorf("capabilities.engines mangled: %v", caps["engines"])
		}
	}
}

// TestUpdateManifestNoProjectFlag pins the compose invocation convention: the
// config-apply start path must not reintroduce `-p citadel-<name>` (the
// project-name mismatch that made stops silent no-ops, #528).
func TestUpdateManifestNoProjectFlag(t *testing.T) {
	data, err := os.ReadFile("config_handler.go")
	if err != nil {
		t.Skipf("cannot read source: %v", err)
	}
	if strings.Contains(string(data), `"-p", projectName`) ||
		strings.Contains(string(data), `"-p", "citadel-`) {
		t.Error("config_handler.go passes a compose -p project flag; the standardized convention is NO -p (#528)")
	}
}
