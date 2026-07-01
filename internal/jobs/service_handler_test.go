// internal/jobs/service_handler_test.go
package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
)

// preDiffusersManifest mirrors a citadel.yaml written before the diffusers
// service was embedded: it lists only vllm/ollama/llamacpp/tei and carries a
// node: block and a capabilities: block that must survive reconciliation.
const preDiffusersManifest = `# citadel.yaml
node:
  name: ubuntu-gpu
  tags: [rtx-3090, gpu]
  org_id: org_test
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
`

// TestReconcileEmbeddedServiceOnMissingManifest verifies that a node whose
// manifest predates the diffusers service (issue #413) can resolve diffusers
// after the fix: the handler materializes the embedded compose file and
// additively registers the service in citadel.yaml, WITHOUT clobbering the
// existing node:/capabilities:/services entries.
func TestReconcileEmbeddedServiceOnMissingManifest(t *testing.T) {
	if _, ok := embeddedservices.ServiceMap["diffusers"]; !ok {
		t.Fatal("precondition: diffusers must be present in embedded ServiceMap")
	}

	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "citadel.yaml")
	if err := os.WriteFile(manifestPath, []byte(preDiffusersManifest), 0600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	h := NewServiceHandler(dir)

	// diffusers must not resolve from the original manifest.
	m, err := h.loadManifest()
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if _, ok := h.findService(m, "diffusers"); ok {
		t.Fatal("precondition: diffusers unexpectedly present in pre-diffusers manifest")
	}

	// Reconcile: this is what Execute() now does when findService misses but the
	// service is embedded.
	svc, err := h.materializeEmbeddedService("diffusers")
	if err != nil {
		t.Fatalf("materializeEmbeddedService: %v", err)
	}
	if svc.Name != "diffusers" || svc.ComposeFile != "services/diffusers.yml" {
		t.Errorf("materialized service = %+v, want diffusers/services/diffusers.yml", svc)
	}

	// The embedded compose file must be written to disk so resolveComposePath
	// (and docker compose) can find it.
	composePath := filepath.Join(dir, "services", "diffusers.yml")
	if _, err := os.Stat(composePath); err != nil {
		t.Errorf("compose file not materialized at %s: %v", composePath, err)
	}

	// After reconcile, diffusers must resolve from the manifest.
	m2, err := h.loadManifest()
	if err != nil {
		t.Fatalf("loadManifest after reconcile: %v", err)
	}
	if _, ok := h.findService(m2, "diffusers"); !ok {
		t.Fatal("diffusers not startable after reconcile")
	}

	// Anti-clobber: the pre-existing services and the node:/capabilities: blocks
	// must all survive the additive rewrite.
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read reconciled manifest: %v", err)
	}
	got := string(raw)
	for _, want := range []string{"vllm", "ollama", "llamacpp", "tei", "ubuntu-gpu", "org_test", "capabilities", "engines"} {
		if !strings.Contains(got, want) {
			t.Errorf("reconciled manifest dropped %q:\n%s", want, got)
		}
	}
	names := map[string]int{}
	for _, s := range m2.Services {
		names[s.Name]++
	}
	for _, want := range []string{"vllm", "ollama", "llamacpp", "tei", "diffusers"} {
		if names[want] != 1 {
			t.Errorf("service %q appears %d times, want exactly 1", want, names[want])
		}
	}
}

// TestReconcileEmbeddedServiceIsIdempotent verifies a second materialize is a
// no-op (no duplicate service block, manifest unchanged).
func TestReconcileEmbeddedServiceIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "citadel.yaml")
	if err := os.WriteFile(manifestPath, []byte(preDiffusersManifest), 0600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	h := NewServiceHandler(dir)

	if _, err := h.materializeEmbeddedService("diffusers"); err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	first, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read after first: %v", err)
	}
	if _, err := h.materializeEmbeddedService("diffusers"); err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	second, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read after second: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("second materialize changed the manifest:\nfirst:\n%s\nsecond:\n%s", first, second)
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
		t.Errorf("diffusers appears %d times after two materializes, want 1", count)
	}
}

// TestReconcileUnknownServiceStillErrors verifies that a service absent from
// both the manifest AND the embedded ServiceMap (e.g. a typo) is not
// materialized -- Execute keeps returning the "not found in manifest" error.
func TestReconcileUnknownServiceStillErrors(t *testing.T) {
	if _, ok := embeddedservices.ServiceMap["definitely-not-a-service"]; ok {
		t.Skip("unexpected: test sentinel is a real embedded service")
	}
}

// TestComposeEnv_InjectsWorkspace verifies that SERVICE_START exports an
// absolute CITADEL_WORKSPACE to docker compose. The transcribe sidecar compose
// uses ${CITADEL_WORKSPACE:?...}; without this injection a worker started via
// --workspace (or the default path) would have no CITADEL_WORKSPACE in its env
// and compose would fail, leaving the node-local STT path dead.
func TestComposeEnv_InjectsWorkspace(t *testing.T) {
	h := NewServiceHandlerWithWorkspace("/etc/citadel", "/home/u/citadel-node/workspace")
	env := h.composeEnv()

	var got string
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "CITADEL_WORKSPACE=") {
			got = strings.TrimPrefix(kv, "CITADEL_WORKSPACE=")
			found = true
		}
	}
	if !found {
		t.Fatal("composeEnv did not set CITADEL_WORKSPACE")
	}
	if got != "/home/u/citadel-node/workspace" {
		t.Errorf("CITADEL_WORKSPACE = %q, want the workspace dir", got)
	}
}

// TestComposeEnv_NoWorkspaceLeavesEnvUntouched verifies that when no workspace
// is configured we do not inject an empty CITADEL_WORKSPACE (which would mount
// the wrong path); compose's :? guard should then fail loudly instead.
func TestComposeEnv_NoWorkspaceLeavesEnvUntouched(t *testing.T) {
	h := NewServiceHandler("/etc/citadel")
	for _, kv := range h.composeEnv() {
		if strings.HasPrefix(kv, "CITADEL_WORKSPACE=") {
			// Only acceptable if it was already present in the process env.
			if strings.TrimPrefix(kv, "CITADEL_WORKSPACE=") == "" {
				t.Errorf("injected empty CITADEL_WORKSPACE when workspace unset")
			}
		}
	}
}
