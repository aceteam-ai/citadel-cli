package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCatalogServicesLayout verifies that the readers resolve services stored
// under the catalog repo's services/<name>/ layout (matching
// aceteam-ai/citadel-services), not a flat <catalog>/<name>/ layout. Regression
// test for #350.
func TestCatalogServicesLayout(t *testing.T) {
	// Hermetic: point ConfigDir at a temp HOME (Linux/darwin non-root path),
	// mirroring TestLockfileUpsert.
	t.Setenv("HOME", t.TempDir())

	// Seed a service under <catalog>/services/<name>/ as the real repo does.
	svcDir := filepath.Join(GetCatalogPath(), "services", "vllm")
	if err := os.MkdirAll(svcDir, 0755); err != nil {
		t.Fatalf("mkdir services dir: %v", err)
	}
	manifest := "name: vllm\nversion: 1.0.0\ndescription: vLLM inference server\ncategory: inference\n"
	if err := os.WriteFile(filepath.Join(svcDir, "service.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatalf("write service.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, "compose.yml"), []byte("services: {}\n"), 0644); err != nil {
		t.Fatalf("write compose.yml: %v", err)
	}

	// LoadServiceManifest must resolve the services/<name>/ manifest.
	m, err := LoadServiceManifest("vllm")
	if err != nil {
		t.Fatalf("LoadServiceManifest(vllm): %v", err)
	}
	if m.Name != "vllm" {
		t.Errorf("manifest name = %q, want vllm", m.Name)
	}

	// GetComposeFile must resolve the services/<name>/compose.yml.
	if _, err := GetComposeFile("vllm"); err != nil {
		t.Errorf("GetComposeFile(vllm): %v", err)
	}

	// scanForServices (registry.yaml-absent fallback) must find it too.
	reg, err := scanForServices(GetCatalogPath())
	if err != nil {
		t.Fatalf("scanForServices: %v", err)
	}
	var found bool
	for _, e := range reg.Services {
		if e.Name == "vllm" {
			found = true
		}
	}
	if !found {
		t.Errorf("scanForServices did not return vllm; got %d services", len(reg.Services))
	}

	// A flat <catalog>/<name>/service.yaml must NOT resolve.
	if _, err := LoadServiceManifest("nonexistent"); err == nil {
		t.Error("expected error for missing service, got nil")
	}
}
