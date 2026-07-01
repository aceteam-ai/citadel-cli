// internal/jobs/service_reconcile_test.go
package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	embedded "github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// preDiffusersManifest is a manifest written before the diffusers service
// existed (v2.55.0). It lists the four legacy serving services plus a
// hand-added, non-embedded "tei" entry (to prove reconcile is additive and
// preserves unknown operator entries).
const preDiffusersManifest = `node:
  name: test-node
  tags:
    - gpu
  org_id: org-123
services:
  - name: vllm
    compose_file: ./services/vllm.yml
  - name: ollama
    compose_file: ./services/ollama.yml
  - name: llamacpp
    compose_file: ./services/llamacpp.yml
  - name: tei
    compose_file: ./services/tei.yml
capabilities:
  engines:
    - vllm
    - ollama
`

func writePreDiffusersManifest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "citadel.yaml"), []byte(preDiffusersManifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

// TestMaterializeEmbeddedService_AddsDiffusers verifies bug A (citadel-cli#413):
// a node whose manifest predates diffusers can, on an on-demand SERVICE_START,
// self-heal by materializing the embedded compose + manifest block so the
// "known service" check passes.
func TestMaterializeEmbeddedService_AddsDiffusers(t *testing.T) {
	dir := writePreDiffusersManifest(t)
	h := NewServiceHandler(dir)

	// Sanity: diffusers is NOT in the manifest to begin with.
	m0, err := h.loadManifest()
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if _, ok := h.findService(m0, "diffusers"); ok {
		t.Fatal("precondition failed: diffusers should be absent from the pre-diffusers manifest")
	}

	svc, err := h.materializeEmbeddedService("diffusers")
	if err != nil {
		t.Fatalf("materializeEmbeddedService: %v", err)
	}
	if svc == nil {
		t.Fatal("materializeEmbeddedService returned nil for an embedded service")
	}
	if svc.Name != "diffusers" {
		t.Errorf("materialized service name = %q, want diffusers", svc.Name)
	}

	// The embedded compose file was written to disk.
	composePath := filepath.Join(dir, "services", "diffusers.yml")
	data, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("expected materialized compose file: %v", err)
	}
	if string(data) != embedded.ServiceMap["diffusers"] {
		t.Error("materialized compose content does not match the embedded template")
	}

	// Reloading the manifest now finds diffusers -> the "known service" check passes.
	m1, err := h.loadManifest()
	if err != nil {
		t.Fatalf("reload manifest: %v", err)
	}
	if _, ok := h.findService(m1, "diffusers"); !ok {
		t.Fatalf("diffusers not found after materialize (known: %s)", h.knownServiceNames(m1))
	}

	// Additive: the pre-existing, non-embedded "tei" entry survives.
	if _, ok := h.findService(m1, "tei"); !ok {
		t.Error("reconcile dropped the non-embedded operator entry 'tei'")
	}
	for _, name := range []string{"vllm", "ollama", "llamacpp"} {
		if _, ok := h.findService(m1, name); !ok {
			t.Errorf("reconcile dropped legacy service %q", name)
		}
	}

	// Non-service manifest content (node, capabilities) is preserved.
	raw, _ := os.ReadFile(filepath.Join(dir, "citadel.yaml"))
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse rewritten manifest: %v", err)
	}
	node, _ := doc["node"].(map[string]any)
	if node == nil || node["org_id"] != "org-123" {
		t.Errorf("node.org_id not preserved after reconcile: %v", doc["node"])
	}
	if _, ok := doc["capabilities"]; !ok {
		t.Error("capabilities section dropped after reconcile")
	}
}

// TestMaterializeEmbeddedService_Idempotent verifies a second materialize is a
// no-op (no duplicate manifest entries, compose content unchanged).
func TestMaterializeEmbeddedService_Idempotent(t *testing.T) {
	dir := writePreDiffusersManifest(t)
	h := NewServiceHandler(dir)

	if _, err := h.materializeEmbeddedService("diffusers"); err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	if _, err := h.materializeEmbeddedService("diffusers"); err != nil {
		t.Fatalf("second materialize: %v", err)
	}

	m, err := h.loadManifest()
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	count := 0
	for _, s := range m.Services {
		if s.Name == "diffusers" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("diffusers appears %d times in manifest, want 1", count)
	}
}

// TestMaterializeEmbeddedService_UnknownService returns (nil, nil) so the caller
// emits the normal "not found in manifest" error rather than fabricating an entry.
func TestMaterializeEmbeddedService_UnknownService(t *testing.T) {
	dir := writePreDiffusersManifest(t)
	h := NewServiceHandler(dir)

	svc, err := h.materializeEmbeddedService("definitely-not-a-real-service")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if svc != nil {
		t.Errorf("expected nil for unknown service, got %+v", svc)
	}
}

// TestMaterializeEmbeddedService_RejectsPathTraversal ensures a crafted service
// name cannot escape the services directory.
func TestMaterializeEmbeddedService_RejectsPathTraversal(t *testing.T) {
	dir := writePreDiffusersManifest(t)
	h := NewServiceHandler(dir)

	svc, err := h.materializeEmbeddedService("../../etc/passwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if svc != nil {
		t.Error("path-traversal service name should be rejected (nil result)")
	}
}

// TestComposeHostPorts_DiffusersPublishesHostPort verifies bug B
// (citadel-cli#415): after the diffusers compose is materialized, the
// SERVICE_START result reports the compose-declared host port so the service is
// reachable on the node. This asserts the intended host-port publish without a
// live docker daemon.
func TestComposeHostPorts_DiffusersPublishesHostPort(t *testing.T) {
	dir := writePreDiffusersManifest(t)
	h := NewServiceHandler(dir)

	svc, err := h.materializeEmbeddedService("diffusers")
	if err != nil {
		t.Fatalf("materializeEmbeddedService: %v", err)
	}

	ports := h.composeHostPorts(*svc)
	if len(ports) == 0 {
		t.Fatal("composeHostPorts returned no host ports for diffusers; " +
			"a started container would come up with no published ports (citadel-cli#415)")
	}

	// Do not hard-depend on a specific number (that is #410's territory); assert
	// the host port matches whatever the embedded compose currently declares.
	wantPort := hostPortFromEmbeddedDiffusers(t)
	found := false
	for _, p := range ports {
		if p == wantPort {
			found = true
		}
	}
	if !found {
		t.Errorf("composeHostPorts = %v, want it to include the compose-declared host port %d", ports, wantPort)
	}
}

// TestServiceStart_ForceRecreateFlag guards the fix that prevents a stale,
// port-less container from being reused: the docker SERVICE_START path must pass
// --force-recreate so the container is (re)created from the current compose
// definition (citadel-cli#415).
func TestServiceStart_ForceRecreateFlag(t *testing.T) {
	src, err := os.ReadFile("service_handler.go")
	if err != nil {
		t.Fatalf("read handler source: %v", err)
	}
	if !strings.Contains(string(src), "--force-recreate") {
		t.Error("SERVICE_START docker path must pass --force-recreate to avoid reusing a port-less stale container")
	}
}

// hostPortFromEmbeddedDiffusers parses the host port out of the embedded
// diffusers compose so the test tracks the compose's current value rather than a
// hardcoded number.
func hostPortFromEmbeddedDiffusers(t *testing.T) int {
	t.Helper()
	var doc struct {
		Services map[string]struct {
			Ports []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(embedded.ServiceMap["diffusers"]), &doc); err != nil {
		t.Fatalf("parse embedded diffusers compose: %v", err)
	}
	svc, ok := doc.Services["diffusers"]
	if !ok || len(svc.Ports) == 0 {
		t.Fatal("embedded diffusers compose declares no ports")
	}
	// "HOST:CONTAINER"
	parts := strings.Split(svc.Ports[0], ":")
	if len(parts) < 2 {
		t.Fatalf("unexpected diffusers port spec: %q", svc.Ports[0])
	}
	var p int
	for _, r := range parts[0] {
		if r < '0' || r > '9' {
			t.Fatalf("non-numeric host port in %q", svc.Ports[0])
		}
		p = p*10 + int(r-'0')
	}
	return p
}
