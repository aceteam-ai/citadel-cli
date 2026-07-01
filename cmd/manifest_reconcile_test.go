// cmd/manifest_reconcile_test.go
package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// setupReconcileHome wires a fake HOME so platform.ConfigDir() resolves to a
// temp .citadel-cli, writes the global config pointing at a node dir, and seeds
// that node dir with the given manifest YAML. It returns the node config dir.
//
// This mirrors the real on-disk layout the reconcile reads through
// findAndReadManifest (global config -> node_config_dir -> citadel.yaml).
func setupReconcileHome(t *testing.T, manifestYAML string) string {
	t.Helper()
	if platform.IsRoot() {
		t.Skip("reconcile test relies on non-root HOME-based ConfigDir")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)

	configDir := platform.ConfigDir() // <home>/.citadel-cli
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	nodeDir := filepath.Join(home, "citadel-node")
	if err := os.MkdirAll(filepath.Join(nodeDir, "services"), 0o755); err != nil {
		t.Fatalf("mkdir node dir: %v", err)
	}

	globalConfig := []byte("node_config_dir: " + nodeDir + "\n")
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), globalConfig, 0o600); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "citadel.yaml"), []byte(manifestYAML), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return nodeDir
}

// preDiffusersManifestYAML lists the legacy serving services plus a hand-added,
// non-embedded "tei" entry (to prove reconcile is additive).
const preDiffusersManifestYAML = `node:
  name: test-node
  tags:
    - gpu
  org_id: org-abc
services:
  - name: vllm
    compose_file: ./services/vllm.yml
  - name: ollama
    compose_file: ./services/ollama.yml
  - name: llamacpp
    compose_file: ./services/llamacpp.yml
  - name: tei
    compose_file: ./services/tei.yml
`

// TestReconcileManifestServices_AddsEmbeddedDiffusers verifies bug A
// (citadel-cli#413): a pre-diffusers manifest reconciles to include every
// embedded service (including diffusers), additively, and materializes each
// compose file.
func TestReconcileManifestServices_AddsEmbeddedDiffusers(t *testing.T) {
	nodeDir := setupReconcileHome(t, preDiffusersManifestYAML)

	added, err := reconcileManifestServices(nodeDir)
	if err != nil {
		t.Fatalf("reconcileManifestServices: %v", err)
	}

	// diffusers must be among the newly-added services.
	foundDiffusers := false
	for _, name := range added {
		if name == "diffusers" {
			foundDiffusers = true
		}
	}
	if !foundDiffusers {
		t.Errorf("reconcile did not add diffusers; added = %v", added)
	}

	manifest, _, err := findAndReadManifest()
	if err != nil {
		t.Fatalf("re-read manifest: %v", err)
	}

	// Every embedded service is now present -> a SERVICE_START "known service"
	// check will pass for all of them.
	for _, name := range services.GetAvailableServices() {
		if !hasService(manifest, name) {
			t.Errorf("embedded service %q missing from manifest after reconcile", name)
		}
	}

	// Additive: legacy + non-embedded operator entries survive.
	for _, name := range []string{"vllm", "ollama", "llamacpp", "tei"} {
		if !hasService(manifest, name) {
			t.Errorf("reconcile dropped pre-existing service %q", name)
		}
	}

	// Node identity preserved.
	if manifest.Node.OrgID != "org-abc" {
		t.Errorf("node.org_id = %q, want org-abc (reconcile clobbered node block)", manifest.Node.OrgID)
	}

	// The embedded compose file was materialized on disk.
	if _, err := os.Stat(filepath.Join(nodeDir, "services", "diffusers.yml")); err != nil {
		t.Errorf("diffusers compose not materialized: %v", err)
	}
}

// TestReconcileManifestServices_Idempotent verifies a second reconcile adds
// nothing and does not duplicate entries.
func TestReconcileManifestServices_Idempotent(t *testing.T) {
	nodeDir := setupReconcileHome(t, preDiffusersManifestYAML)

	if _, err := reconcileManifestServices(nodeDir); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	added, err := reconcileManifestServices(nodeDir)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(added) != 0 {
		t.Errorf("second reconcile added %v, want none (not idempotent)", added)
	}

	// No duplicate diffusers entries.
	raw, _ := os.ReadFile(filepath.Join(nodeDir, "citadel.yaml"))
	var doc struct {
		Services []struct {
			Name string `yaml:"name"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	count := 0
	for _, s := range doc.Services {
		if s.Name == "diffusers" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("diffusers appears %d times after two reconciles, want 1", count)
	}
}

// TestReconcileManifestServices_PreservesOperatorComposeFile verifies reconcile
// never overwrites an operator-authored compose file for a service it also
// embeds.
func TestReconcileManifestServices_PreservesOperatorComposeFile(t *testing.T) {
	// Manifest omits vllm from services so reconcile will re-add it; but the
	// operator already placed a custom vllm.yml on disk.
	const yaml = `node:
  name: test-node
services:
  - name: ollama
    compose_file: ./services/ollama.yml
`
	nodeDir := setupReconcileHome(t, yaml)

	customCompose := "# operator custom vllm\nservices:\n  vllm:\n    image: custom\n"
	vllmPath := filepath.Join(nodeDir, "services", "vllm.yml")
	if err := os.WriteFile(vllmPath, []byte(customCompose), 0o600); err != nil {
		t.Fatalf("write custom compose: %v", err)
	}

	if _, err := reconcileManifestServices(nodeDir); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got, err := os.ReadFile(vllmPath)
	if err != nil {
		t.Fatalf("read vllm compose: %v", err)
	}
	if string(got) != customCompose {
		t.Error("reconcile overwrote an operator-authored compose file")
	}
}
