// cmd/manifest_test.go
package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
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

// TestEnsureComposeFile_NoOverrideWritesVerbatim pins the #860 backward-compat
// contract: with no --node-dir/CITADEL_NODE_DIR override active, the
// materialized compose file must be byte-identical to the embedded template
// -- no container_name rewriting at all.
func TestEnsureComposeFile_NoOverrideWritesVerbatim(t *testing.T) {
	setNodeDirOverrideForTest(t, "")
	tmpDir := t.TempDir()

	if err := ensureComposeFile(tmpDir, "vllm"); err != nil {
		t.Fatalf("ensureComposeFile() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(tmpDir, "services", "vllm.yml"))
	if err != nil {
		t.Fatalf("failed to read materialized compose file: %v", err)
	}
	want := services.ServiceMap["vllm"]
	if string(got) != want {
		t.Fatalf("materialized compose file is not byte-identical to the embedded template without an override")
	}
	if !strings.Contains(string(got), "container_name: citadel-vllm") {
		t.Fatalf("materialized compose file missing the unnamespaced container_name line, got: %s", got)
	}
}

// TestEnsureComposeFile_OverrideNamespacesContainerName pins the #860 fix
// itself: under an active override, the materialized container_name line
// carries the SAME hash embeddedContainerName/composeProjectOverride derive
// for that override dir.
func TestEnsureComposeFile_OverrideNamespacesContainerName(t *testing.T) {
	overrideDir := t.TempDir()
	setNodeDirOverrideForTest(t, overrideDir)
	tmpDir := t.TempDir()

	if err := ensureComposeFile(tmpDir, "vllm"); err != nil {
		t.Fatalf("ensureComposeFile() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(tmpDir, "services", "vllm.yml"))
	if err != nil {
		t.Fatalf("failed to read materialized compose file: %v", err)
	}

	wantName := embeddedContainerName("vllm")
	wantLine := "container_name: " + wantName
	if !strings.Contains(string(got), wantLine) {
		t.Fatalf("materialized compose file = %s, want it to contain %q", got, wantLine)
	}
	if strings.Contains(string(got), "container_name: citadel-vllm\n") {
		t.Fatalf("materialized compose file still contains the UNnamespaced container_name line: %s", got)
	}
	// bonsai-style services also reference "citadel-<svc>" in an image: tag;
	// vllm doesn't, but assert the rewrite didn't touch anything else in the
	// file besides the one container_name line (content is otherwise
	// identical to the embedded template).
	rewrittenBack := strings.Replace(string(got), wantLine, "container_name: citadel-vllm", 1)
	if rewrittenBack != services.ServiceMap["vllm"] {
		t.Fatalf("materialization changed more than just the container_name line")
	}
}

// TestEnsureComposeFile_ExistingUnnamespacedFileIsReconciledUnderOverride
// pins the review fix: a compose file already materialized (by a pre-#860
// binary, or by this binary before an override was set) with the plain
// "citadel-vllm" container_name must be rewritten in place the NEXT time
// ensureComposeFile runs under an active override -- the "file already
// exists, leave it alone" fast path must not leave a stale, colliding name
// on disk forever.
func TestEnsureComposeFile_ExistingUnnamespacedFileIsReconciledUnderOverride(t *testing.T) {
	tmpDir := t.TempDir()

	// First materialize with NO override (the pre-existing, unnamespaced file).
	setNodeDirOverrideForTest(t, "")
	if err := ensureComposeFile(tmpDir, "vllm"); err != nil {
		t.Fatalf("initial ensureComposeFile() error = %v", err)
	}

	// Now re-run against the SAME dir with an override active.
	overrideDir := t.TempDir()
	setNodeDirOverrideForTest(t, overrideDir)
	if err := ensureComposeFile(tmpDir, "vllm"); err != nil {
		t.Fatalf("ensureComposeFile() under override error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(tmpDir, "services", "vllm.yml"))
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	want := "container_name: " + embeddedContainerName("vllm")
	if !strings.Contains(string(got), want) {
		t.Fatalf("existing file was not reconciled to the namespaced name: got %s, want it to contain %q", got, want)
	}
	if strings.Contains(string(got), "container_name: citadel-vllm\n") {
		t.Fatalf("existing file still carries the stale unnamespaced container_name: %s", got)
	}
}

// TestEnsureComposeFile_ExistingCatalogServiceFileUntouchedUnderOverride pins
// the scope boundary from the OTHER direction: ensureComposeFile is also the
// materialization choke point for non-embedded (e.g. catalog-installed)
// manifest services, whose compose files author their own container_name.
// The existing-file reconciliation must not touch (or refuse on) a file for
// a service name that isn't in services.ServiceMap.
func TestEnsureComposeFile_ExistingCatalogServiceFileUntouchedUnderOverride(t *testing.T) {
	setNodeDirOverrideForTest(t, "")
	tmpDir := t.TempDir()
	servicesDir := filepath.Join(tmpDir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const catalogContent = "services:\n  acme-app:\n    image: acme/app\n    container_name: acme-app\n"
	if err := os.WriteFile(filepath.Join(servicesDir, "acme-app.yml"), []byte(catalogContent), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	setNodeDirOverrideForTest(t, t.TempDir())
	if err := ensureComposeFile(tmpDir, "acme-app"); err != nil {
		t.Fatalf("ensureComposeFile() for a non-embedded service under override must not error, got: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(servicesDir, "acme-app.yml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != catalogContent {
		t.Fatalf("a non-embedded service's compose file must be left untouched under --node-dir, got: %s", got)
	}
}

// TestEnsureComposeFile_TwoDifferentOverrideDirsProduceDifferentContainerNames
// is the end-to-end version of the same contract pinned at the unit level in
// internal/compose/nodedir_test.go and cmd/nodedir_test.go: two operators
// running against DIFFERENT --node-dir overrides never materialize a
// colliding container_name for the same embedded service.
func TestEnsureComposeFile_TwoDifferentOverrideDirsProduceDifferentContainerNames(t *testing.T) {
	overrideA := t.TempDir()
	overrideB := t.TempDir()
	nodeDirA := t.TempDir()
	nodeDirB := t.TempDir()

	setNodeDirOverrideForTest(t, overrideA)
	nameA := embeddedContainerName("vllm")
	if err := ensureComposeFile(nodeDirA, "vllm"); err != nil {
		t.Fatalf("ensureComposeFile(A) error = %v", err)
	}
	contentA, err := os.ReadFile(filepath.Join(nodeDirA, "services", "vllm.yml"))
	if err != nil {
		t.Fatalf("read A: %v", err)
	}

	setNodeDirOverrideForTest(t, overrideB)
	nameB := embeddedContainerName("vllm")
	if err := ensureComposeFile(nodeDirB, "vllm"); err != nil {
		t.Fatalf("ensureComposeFile(B) error = %v", err)
	}
	contentB, err := os.ReadFile(filepath.Join(nodeDirB, "services", "vllm.yml"))
	if err != nil {
		t.Fatalf("read B: %v", err)
	}

	if nameA == nameB {
		t.Fatalf("two different --node-dir overrides derived the SAME container name %q for vllm -- they would collide", nameA)
	}
	if !strings.Contains(string(contentA), "container_name: "+nameA) {
		t.Fatalf("materialized file A does not contain the expected container_name %q: %s", nameA, contentA)
	}
	if !strings.Contains(string(contentB), "container_name: "+nameB) {
		t.Fatalf("materialized file B does not contain the expected container_name %q: %s", nameB, contentB)
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

// --- --node-dir / CITADEL_NODE_DIR override (citadel#853) ---

// TestFindAndReadManifest_NoOverride_Unchanged pins backward compatibility:
// with no --node-dir/CITADEL_NODE_DIR set, findAndReadManifest resolves
// exactly as before (via $HOME/.citadel-cli/config.yaml's node_config_dir
// indirection), and platform.ConfigDir() is consulted as usual.
func TestFindAndReadManifest_NoOverride_Unchanged(t *testing.T) {
	nodeDir := writeManifestWithServices(t, []Service{{Name: "vllm", ComposeFile: "./services/vllm.yml"}})

	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		t.Fatalf("findAndReadManifest: %v", err)
	}
	if configDir != nodeDir {
		t.Fatalf("configDir = %q, want %q (the $HOME-resolved node dir)", configDir, nodeDir)
	}
	if !hasService(manifest, "vllm") {
		t.Fatalf("expected vllm in manifest resolved via $HOME")
	}
}

// TestFindAndReadManifest_NodeDirOverride_BypassesHome is the core #853
// acceptance test: --node-dir points manifest resolution at an alternate
// (temp) directory, NOT $HOME -- and crucially, it never even touches
// platform.ConfigDir()/$HOME/.citadel-cli/config.yaml, which is the exact
// incident-prevention property (a later shell call missing the isolated $HOME
// must not silently fall through to the real node's global config).
func TestFindAndReadManifest_NodeDirOverride_BypassesHome(t *testing.T) {
	// "Real" $HOME: has its own global config + manifest, deliberately never
	// read while the override is active.
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	realConfigDir := filepath.Join(realHome, ".citadel-cli")
	if err := os.MkdirAll(realConfigDir, 0o755); err != nil {
		t.Fatalf("mkdir real config dir: %v", err)
	}
	realNodeDir := filepath.Join(realHome, "citadel-node")
	if err := os.MkdirAll(filepath.Join(realNodeDir, "services"), 0o755); err != nil {
		t.Fatalf("mkdir real node dir: %v", err)
	}
	writeYAMLFile(t, filepath.Join(realConfigDir, "config.yaml"), map[string]string{"node_config_dir": realNodeDir})
	writeYAMLFile(t, filepath.Join(realNodeDir, "citadel.yaml"), &CitadelManifest{
		Services: []Service{{Name: "vllm", ComposeFile: "./services/vllm.yml"}},
	})

	// Override target: a completely separate temp dir simulating an agent's
	// isolated test node.
	overrideDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(overrideDir, "services"), 0o755); err != nil {
		t.Fatalf("mkdir override dir: %v", err)
	}
	writeYAMLFile(t, filepath.Join(overrideDir, "citadel.yaml"), &CitadelManifest{
		Services: []Service{{Name: "bonsai", ComposeFile: "./services/bonsai.yml"}},
	})

	setNodeDirOverrideForTest(t, overrideDir)

	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		t.Fatalf("findAndReadManifest with --node-dir: %v", err)
	}
	if configDir != overrideDir {
		t.Fatalf("configDir = %q, want the override dir %q", configDir, overrideDir)
	}
	if !hasService(manifest, "bonsai") {
		t.Fatalf("expected bonsai (the OVERRIDE dir's service) in the resolved manifest")
	}
	if hasService(manifest, "vllm") {
		t.Fatalf("resolved manifest must NOT be the real $HOME manifest (it declares vllm, not bonsai)")
	}

	// The incident-prevention property: the real machine's global config.yaml
	// was never even read (let alone written) while the override is active.
	realConfigData, err := os.ReadFile(filepath.Join(realConfigDir, "config.yaml"))
	if err != nil {
		t.Fatalf("real config.yaml should still exist untouched: %v", err)
	}
	if !strings.Contains(string(realConfigData), realNodeDir) {
		t.Fatalf("real config.yaml content changed unexpectedly: %s", realConfigData)
	}
}

// TestFindAndReadManifest_NodeDirOverride_ViaEnvVar mirrors the flag test but
// through CITADEL_NODE_DIR, and confirms the flag wins when both are set.
func TestFindAndReadManifest_NodeDirOverride_ViaEnvVar(t *testing.T) {
	envDir := t.TempDir()
	writeYAMLFile(t, filepath.Join(envDir, "citadel.yaml"), &CitadelManifest{
		Services: []Service{{Name: "ollama", ComposeFile: "./services/ollama.yml"}},
	})
	t.Setenv("CITADEL_NODE_DIR", envDir)
	t.Cleanup(func() { nodeDirFlag = "" })

	_, configDir, err := findAndReadManifest()
	if err != nil {
		t.Fatalf("findAndReadManifest with CITADEL_NODE_DIR: %v", err)
	}
	if configDir != envDir {
		t.Fatalf("configDir = %q, want the env-var dir %q", configDir, envDir)
	}

	// Flag wins over env var when both are set.
	flagDir := t.TempDir()
	writeYAMLFile(t, filepath.Join(flagDir, "citadel.yaml"), &CitadelManifest{
		Services: []Service{{Name: "kokoro", ComposeFile: "./services/kokoro.yml"}},
	})
	setNodeDirOverrideForTest(t, flagDir)

	_, configDir, err = findAndReadManifest()
	if err != nil {
		t.Fatalf("findAndReadManifest with both set: %v", err)
	}
	if configDir != flagDir {
		t.Fatalf("configDir = %q, want the FLAG dir %q (flag must win over env var)", configDir, flagDir)
	}
}

// TestFindAndReadManifest_NodeDirOverride_MissingManifestErrors confirms the
// override path does not silently fall back to $HOME when the override
// directory itself has no manifest -- exactly the failure mode this feature
// exists to prevent (a missing/typo'd override must error, never quietly
// resolve somewhere else).
func TestFindAndReadManifest_NodeDirOverride_MissingManifestErrors(t *testing.T) {
	setNodeDirOverrideForTest(t, t.TempDir()) // empty dir, no citadel.yaml

	_, _, err := findAndReadManifest()
	if err == nil {
		t.Fatal("want an error when the override dir has no manifest")
	}
	if !strings.Contains(err.Error(), "--node-dir") {
		t.Fatalf("error = %v, want it to mention --node-dir", err)
	}
}

// TestFindOrCreateManifest_NodeDirOverride_BootstrapsThereNotHome confirms
// findOrCreateManifest bootstraps a fresh manifest AT the override directory
// (not $HOME/citadel-node) and does NOT write the machine-wide global config
// -- an override is a one-off target, not a new permanent default.
func TestFindOrCreateManifest_NodeDirOverride_BootstrapsThereNotHome(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)

	overrideDir := filepath.Join(t.TempDir(), "isolated-node")
	setNodeDirOverrideForTest(t, overrideDir)

	manifest, configDir, err := findOrCreateManifest()
	if err != nil {
		t.Fatalf("findOrCreateManifest with --node-dir: %v", err)
	}
	if configDir != overrideDir {
		t.Fatalf("configDir = %q, want the override dir %q", configDir, overrideDir)
	}
	if manifest == nil {
		t.Fatal("want a bootstrapped manifest")
	}
	if _, err := os.Stat(filepath.Join(overrideDir, "citadel.yaml")); err != nil {
		t.Fatalf("expected citadel.yaml written at override dir: %v", err)
	}

	// The real machine's global config must NOT have been created/pointed at
	// this override.
	if _, err := os.Stat(filepath.Join(realHome, ".citadel-cli", "config.yaml")); err == nil {
		t.Fatalf("global config.yaml must not be written under $HOME while --node-dir is active")
	}
	if _, err := os.Stat(filepath.Join(realHome, "citadel-node")); err == nil {
		t.Fatalf("$HOME/citadel-node must not be created while --node-dir is active")
	}
}

// writeYAMLFile marshals v to YAML and writes it to path, creating parent
// directories as needed. Test helper shared by the --node-dir override tests.
func writeYAMLFile(t *testing.T, path string, v interface{}) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	data, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal yaml for %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
