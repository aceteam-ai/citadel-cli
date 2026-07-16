// cmd/manifest_test.go
package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestHasService(t *testing.T) {
	manifest := &CitadelManifest{
		Services: []Service{
			{Name: "vllm", ComposeFile: "./services/vllm.yml"},
			{Name: "ollama", ComposeFile: "./services/ollama.yml"},
		},
	}

	tests := []struct {
		name        string
		serviceName string
		want        bool
	}{
		{"existing service vllm", "vllm", true},
		{"existing service ollama", "ollama", true},
		{"non-existing service", "llamacpp", false},
		{"empty string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasService(manifest, tt.serviceName)
			if got != tt.want {
				t.Errorf("hasService() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWriteManifest(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "citadel-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	manifestPath := filepath.Join(tmpDir, "citadel.yaml")

	manifest := &CitadelManifest{
		Node: struct {
			Name  string   `yaml:"name"`
			Tags  []string `yaml:"tags"`
			OrgID string   `yaml:"org_id,omitempty"`
		}{
			Name: "test-node",
			Tags: []string{"test", "gpu"},
		},
		Services: []Service{
			{Name: "vllm", ComposeFile: "./services/vllm.yml"},
		},
	}

	// Write the manifest
	err = writeManifest(manifestPath, manifest)
	if err != nil {
		t.Fatalf("writeManifest() error = %v", err)
	}

	// Read it back and verify
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("Failed to read written manifest: %v", err)
	}

	var readManifest CitadelManifest
	err = yaml.Unmarshal(data, &readManifest)
	if err != nil {
		t.Fatalf("Failed to unmarshal manifest: %v", err)
	}

	if readManifest.Node.Name != "test-node" {
		t.Errorf("Node.Name = %q, want %q", readManifest.Node.Name, "test-node")
	}

	if len(readManifest.Services) != 1 {
		t.Errorf("len(Services) = %d, want 1", len(readManifest.Services))
	}

	if readManifest.Services[0].Name != "vllm" {
		t.Errorf("Services[0].Name = %q, want %q", readManifest.Services[0].Name, "vllm")
	}
}

func TestEnsureComposeFile(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "citadel-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Test creating a compose file for a known service
	err = ensureComposeFile(tmpDir, "vllm")
	if err != nil {
		t.Fatalf("ensureComposeFile() error = %v", err)
	}

	// Verify the file was created
	composePath := filepath.Join(tmpDir, "services", "vllm.yml")
	if _, err := os.Stat(composePath); os.IsNotExist(err) {
		t.Errorf("Compose file was not created at %s", composePath)
	}

	// Test that calling again doesn't fail (idempotent)
	err = ensureComposeFile(tmpDir, "vllm")
	if err != nil {
		t.Errorf("ensureComposeFile() second call error = %v", err)
	}

	// Test unknown service
	err = ensureComposeFile(tmpDir, "unknown-service")
	if err == nil {
		t.Error("ensureComposeFile() expected error for unknown service, got nil")
	}
}

func TestAddServiceToManifest(t *testing.T) {
	// Create a temporary directory structure
	tmpDir, err := os.MkdirTemp("", "citadel-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create the config directory structure
	configDir := filepath.Join(tmpDir, "citadel-node")
	servicesDir := filepath.Join(configDir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatalf("Failed to create services dir: %v", err)
	}

	// Create a global config pointing to this directory
	globalConfigDir := filepath.Join(tmpDir, "etc", "citadel")
	if err := os.MkdirAll(globalConfigDir, 0755); err != nil {
		t.Fatalf("Failed to create global config dir: %v", err)
	}
	globalConfigPath := filepath.Join(globalConfigDir, "config.yaml")
	globalConfig := []byte("node_config_dir: " + configDir + "\n")
	if err := os.WriteFile(globalConfigPath, globalConfig, 0644); err != nil {
		t.Fatalf("Failed to write global config: %v", err)
	}

	// Create initial manifest
	manifestPath := filepath.Join(configDir, "citadel.yaml")
	initialManifest := &CitadelManifest{
		Node: struct {
			Name  string   `yaml:"name"`
			Tags  []string `yaml:"tags"`
			OrgID string   `yaml:"org_id,omitempty"`
		}{
			Name: "test-node",
			Tags: []string{},
		},
		Services: []Service{},
	}
	if err := writeManifest(manifestPath, initialManifest); err != nil {
		t.Fatalf("Failed to write initial manifest: %v", err)
	}

	// Note: addServiceToManifest uses findAndReadManifest which requires
	// the global config to be in the correct location. This test would
	// need to mock the platform.ConfigDir() function to work properly.
	// For now, we'll skip this specific test and rely on integration tests.
	t.Skip("Skipping addServiceToManifest test - requires platform.ConfigDir() mock")
}

// TestStripTags covers the uninstall tag-symmetry cleanup (#514): removing a
// module must drop the node_tags it declared, preserving order and any tags it
// did not contribute.
func TestStripTags(t *testing.T) {
	cases := []struct {
		name   string
		tags   []string
		remove []string
		want   []string
	}{
		{
			name:   "removes the module's declared tag",
			tags:   []string{"cpu:general", "meeting", "os:linux"},
			remove: []string{"meeting"},
			want:   []string{"cpu:general", "os:linux"},
		},
		{
			name:   "no-op when nothing to remove",
			tags:   []string{"cpu:general", "meeting"},
			remove: nil,
			want:   []string{"cpu:general", "meeting"},
		},
		{
			name:   "removes multiple declared tags",
			tags:   []string{"a", "meeting", "notetaker", "b"},
			remove: []string{"meeting", "notetaker"},
			want:   []string{"a", "b"},
		},
		{
			name:   "tag not present is a no-op",
			tags:   []string{"cpu:general"},
			remove: []string{"meeting"},
			want:   []string{"cpu:general"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripTags(c.tags, c.remove)
			if len(got) != len(c.want) {
				t.Fatalf("stripTags = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("stripTags = %v, want %v", got, c.want)
				}
			}
		})
	}
}
