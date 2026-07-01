// cmd/manifest.go
package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// Service defines a single managed service.
type Service struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type,omitempty"`         // "native" or "docker" (default: auto-detect)
	ComposeFile string `yaml:"compose_file,omitempty"` // For docker services
	Port        int    `yaml:"port,omitempty"`         // For native services
}

// ManifestCapabilities defines the optional capabilities section in citadel.yaml.
// If not declared, capabilities are auto-detected at startup.
type ManifestCapabilities struct {
	GPUs    []ManifestGPU `yaml:"gpus,omitempty"`
	Engines []string      `yaml:"engines,omitempty"` // inference engines: vllm, sglang, ollama, llamacpp
}

// ManifestGPU describes a GPU declared in the manifest.
type ManifestGPU struct {
	Name   string `yaml:"name"`              // e.g. "NVIDIA GeForce RTX 3090"
	VRAMMb int    `yaml:"vram_mb,omitempty"` // e.g. 24576
	Count  int    `yaml:"count,omitempty"`   // defaults to 1
}

// CitadelManifest defines the structure of the citadel.yaml file.
type CitadelManifest struct {
	Node struct {
		Name  string   `yaml:"name"`
		Tags  []string `yaml:"tags"`
		OrgID string   `yaml:"org_id,omitempty"`
	} `yaml:"node"`
	Services     []Service             `yaml:"services"`
	Capabilities *ManifestCapabilities `yaml:"capabilities,omitempty"`
}

// findAndReadManifest locates and parses the node's manifest file.
// It exclusively uses the global config file as the single source of truth for
// locating the node's configuration directory. This ensures consistent behavior
// regardless of the current working directory.
func findAndReadManifest() (*CitadelManifest, string, error) {
	globalConfigFile := filepath.Join(platform.ConfigDir(), "config.yaml")

	// Step 1: Read the global config file to find the node's directory.
	globalConfigData, err := os.ReadFile(globalConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("global config not found at %s. Please run 'citadel init'", globalConfigFile)
		}
		return nil, "", fmt.Errorf("could not read global config %s: %w", globalConfigFile, err)
	}

	var globalConf struct {
		NodeConfigDir string `yaml:"node_config_dir"`
	}
	if err := yaml.Unmarshal(globalConfigData, &globalConf); err != nil {
		return nil, "", fmt.Errorf("could not parse global config %s: %w", globalConfigFile, err)
	}

	if globalConf.NodeConfigDir == "" {
		// Try to auto-fix by checking default location
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, "", fmt.Errorf("global config %s is invalid: missing 'node_config_dir'", globalConfigFile)
		}

		defaultNodeDir := filepath.Join(homeDir, "citadel-node")
		defaultManifest := filepath.Join(defaultNodeDir, "citadel.yaml")

		if _, err := os.Stat(defaultManifest); err == nil {
			// Found manifest in default location - auto-fix the config
			globalConf.NodeConfigDir = defaultNodeDir

			// Read existing config to preserve other fields. A successful
			// unmarshal of an empty/whitespace/null file yields a nil map (e.g.
			// when the config was truncated by a disk-full event), so guard
			// against nil before writing or the assignment below panics.
			var config map[string]interface{}
			if err := yaml.Unmarshal(globalConfigData, &config); err != nil || config == nil {
				config = make(map[string]interface{})
			}
			config["node_config_dir"] = defaultNodeDir

			// Write back
			if newData, err := yaml.Marshal(config); err == nil {
				_ = os.WriteFile(globalConfigFile, newData, 0600)
			}
		} else {
			return nil, "", fmt.Errorf("global config %s is invalid: missing 'node_config_dir'", globalConfigFile)
		}
	}

	// Step 2: Load the manifest from the path specified in the global config.
	nodeConfigDir := globalConf.NodeConfigDir
	manifestPath := filepath.Join(nodeConfigDir, "citadel.yaml")

	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("manifest not found at %s. Run 'citadel init' to regenerate the configuration", manifestPath)
		}
		return nil, "", fmt.Errorf("could not read manifest from global path %s: %w", manifestPath, err)
	}

	var manifest CitadelManifest
	if err := yaml.Unmarshal(manifestData, &manifest); err != nil {
		return nil, "", fmt.Errorf("could not parse manifest from global path %s: %w", manifestPath, err)
	}

	// Return the manifest and the absolute path to its directory.
	return &manifest, nodeConfigDir, nil
}

// findOrCreateManifest returns the manifest if it exists, or creates a bootstrap
// configuration if it doesn't. This enables `citadel run` to work without `citadel init`.
func findOrCreateManifest() (*CitadelManifest, string, error) {
	// Try to find existing manifest
	manifest, configDir, err := findAndReadManifest()
	if err == nil {
		return manifest, configDir, nil
	}

	// No manifest found - bootstrap a minimal configuration
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get home directory: %w", err)
	}

	configDir = filepath.Join(homeDir, "citadel-node")
	servicesDir := filepath.Join(configDir, "services")
	manifestPath := filepath.Join(configDir, "citadel.yaml")

	// Create directories
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return nil, "", fmt.Errorf("failed to create config directory: %w", err)
	}

	// Get hostname for node name
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "citadel-node"
	}

	// Create minimal manifest
	manifest = &CitadelManifest{
		Node: struct {
			Name  string   `yaml:"name"`
			Tags  []string `yaml:"tags"`
			OrgID string   `yaml:"org_id,omitempty"`
		}{
			Name: hostname,
			Tags: []string{},
		},
		Services: []Service{},
	}

	// Write manifest
	if err := writeManifest(manifestPath, manifest); err != nil {
		return nil, "", err
	}

	// Create global config pointing to this directory
	if err := writeGlobalConfig(configDir); err != nil {
		return nil, "", err
	}

	fmt.Printf("✅ Created new configuration at %s\n", configDir)
	return manifest, configDir, nil
}

// writeManifest writes the manifest to disk.
func writeManifest(path string, manifest *CitadelManifest) error {
	yamlData, err := yaml.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}
	if err := os.WriteFile(path, yamlData, 0600); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}
	return nil
}

// writeGlobalConfig creates the global config file pointing to the node's config directory.
func writeGlobalConfig(nodeConfigDir string) error {
	globalConfigDir := platform.ConfigDir()
	globalConfigFile := filepath.Join(globalConfigDir, "config.yaml")

	if err := os.MkdirAll(globalConfigDir, 0755); err != nil {
		return fmt.Errorf("failed to create global config directory %s: %w", globalConfigDir, err)
	}

	configContent := fmt.Sprintf("node_config_dir: %s\n", nodeConfigDir)
	if err := os.WriteFile(globalConfigFile, []byte(configContent), 0600); err != nil {
		return fmt.Errorf("failed to write global config file %s: %w", globalConfigFile, err)
	}
	return nil
}

// hasService checks if a service is already in the manifest.
func hasService(manifest *CitadelManifest, serviceName string) bool {
	for _, s := range manifest.Services {
		if s.Name == serviceName {
			return true
		}
	}
	return false
}

// addServiceToManifest adds a service to the manifest and writes it to disk,
// honoring the hardcoded capability-tag map for embedded/catalog services.
func addServiceToManifest(configDir, serviceName string) error {
	return addServiceToManifestWithTags(configDir, serviceName, nil)
}

// addServiceToManifestWithTags adds a service to the manifest and writes it to
// disk. In addition to the hardcoded capability-tag map (back-compat for
// embedded/catalog services), it merges any module-declared routing tags
// (service.yaml's node_tags) into Node.Tags, so third-party engines become
// routable without a CLI change. Tags are deduped via containsTag.
func addServiceToManifestWithTags(configDir, serviceName string, nodeTags []string) error {
	manifestPath := filepath.Join(configDir, "citadel.yaml")

	// Read existing manifest
	manifest, _, err := findAndReadManifest()
	if err != nil {
		return fmt.Errorf("failed to read manifest: %w", err)
	}

	// Check if already exists
	if hasService(manifest, serviceName) {
		return nil // Already present
	}

	// Add new service
	manifest.Services = append(manifest.Services, Service{
		Name:        serviceName,
		ComposeFile: filepath.Join("./services", serviceName+".yml"),
	})

	// Auto-add capability tags for specific embedded/catalog services (back-compat).
	serviceTags := map[string][]string{
		"extraction": {"extraction:gliner2", "model:gliner2-base-v1"},
	}
	if tags, ok := serviceTags[serviceName]; ok {
		for _, tag := range tags {
			if !containsTag(manifest.Node.Tags, tag) {
				manifest.Node.Tags = append(manifest.Node.Tags, tag)
			}
		}
	}

	// Merge module-declared routing tags (service.yaml node_tags).
	for _, tag := range nodeTags {
		if tag != "" && !containsTag(manifest.Node.Tags, tag) {
			manifest.Node.Tags = append(manifest.Node.Tags, tag)
		}
	}

	// Write back
	return writeManifest(manifestPath, manifest)
}

// reconcileManifestServices makes the on-disk manifest consistent with the
// services this binary can deploy. For every service embedded in the binary
// (services.ServiceMap) that is missing from the manifest's services list, it
// adds an additive service block and materializes the embedded compose file
// into <configDir>/services/<name>.yml.
//
// This closes the "advertised-but-unstartable" gap (citadel-cli#413): when a
// newer binary embeds a service (e.g. diffusers in v2.55.0) and advertises it in
// the heartbeat, but the node's citadel.yaml was written at init time before that
// service existed, a SERVICE_START would be rejected with "service not found in
// manifest". A binary upgrade does not otherwise touch the manifest, so the node
// keeps advertising a capability it cannot start until reconciled here.
//
// Guarantees:
//   - Additive-only: never removes or overwrites operator-defined entries. An
//     existing service (matched by name) is left untouched, including
//     operator-authored compose files (ensureComposeFile is a no-op when the file
//     already exists).
//   - Preserves unknown entries: services in the manifest that are not embedded
//     (e.g. a hand-added "tei") are retained as-is.
//   - Idempotent: a second call is a no-op.
//
// It returns the names of services newly added (for logging). It never returns an
// error that should abort startup; failures for a single service are collected
// and surfaced, but the manifest write only includes services that materialized
// cleanly.
//
// IMPORTANT ordering note for callers: this must run AFTER startManagedServices
// so newly-reconciled embedded services are not auto-started on this boot. The
// manifest lists only operator intent for the purposes of auto-start (#384: never
// gate boot on heavy GPU services); reconciled entries exist so an on-demand
// SERVICE_START can find them, not so they launch unbidden.
func reconcileManifestServices(configDir string) (added []string, err error) {
	manifestPath := filepath.Join(configDir, "citadel.yaml")

	manifest, _, mErr := findAndReadManifest()
	if mErr != nil {
		return nil, fmt.Errorf("failed to read manifest for reconcile: %w", mErr)
	}

	// Iterate embedded services in a stable order so logs and writes are
	// deterministic.
	changed := false
	for _, svcName := range services.GetAvailableServices() {
		if hasService(manifest, svcName) {
			continue // operator/prior-reconcile entry -- leave untouched
		}

		// Materialize the embedded compose file (no-op if it already exists).
		if cErr := ensureComposeFile(configDir, svcName); cErr != nil {
			err = errors.Join(err, fmt.Errorf("reconcile %s: %w", svcName, cErr))
			continue
		}

		manifest.Services = append(manifest.Services, Service{
			Name:        svcName,
			ComposeFile: filepath.Join("./services", svcName+".yml"),
		})
		added = append(added, svcName)
		changed = true
	}

	if !changed {
		return added, err
	}

	if wErr := writeManifest(manifestPath, manifest); wErr != nil {
		return added, errors.Join(err, fmt.Errorf("failed to write reconciled manifest: %w", wErr))
	}

	return added, err
}

// containsTag checks if a tag is already in the tags slice.
func containsTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// ensureComposeFile ensures the compose file exists in the services directory.
// If it doesn't exist, it extracts the embedded compose file from the binary.
func ensureComposeFile(configDir, serviceName string) error {
	servicesDir := filepath.Join(configDir, "services")
	destPath := filepath.Join(servicesDir, serviceName+".yml")

	// Check if file already exists
	if _, err := os.Stat(destPath); err == nil {
		return nil // Already exists
	}

	// Get content from embedded services
	content, ok := services.ServiceMap[serviceName]
	if !ok {
		return fmt.Errorf("unknown service: %s", serviceName)
	}

	// Ensure services directory exists
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return fmt.Errorf("failed to create services directory: %w", err)
	}

	// Write compose file (0600 to protect any sensitive env vars)
	if err := os.WriteFile(destPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to write compose file: %w", err)
	}

	return nil
}
