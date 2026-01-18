// cmd/manifest.go
package cmd

import (
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
	Type        string `yaml:"type,omitempty"`        // "native" or "docker" (default: auto-detect)
	ComposeFile string `yaml:"compose_file,omitempty"` // For docker services
	Port        int    `yaml:"port,omitempty"`         // For native services
}

// CitadelManifest defines the structure of the citadel.yaml file.
type CitadelManifest struct {
	Node struct {
		Name  string   `yaml:"name"`
		Tags  []string `yaml:"tags"`
		OrgID string   `yaml:"org_id,omitempty"`
	} `yaml:"node"`
	Services []Service `yaml:"services"`
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
		return nil, "", fmt.Errorf("global config %s is invalid: missing 'node_config_dir'", globalConfigFile)
	}

	// Step 2: Load the manifest from the path specified in the global config.
	nodeConfigDir := globalConf.NodeConfigDir
	manifestPath := filepath.Join(nodeConfigDir, "citadel.yaml")

	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("manifest not found at %s. The configuration is incomplete or corrupt", manifestPath)
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

// addServiceToManifest adds a service to the manifest and writes it to disk.
func addServiceToManifest(configDir, serviceName string) error {
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

	// Write back
	return writeManifest(manifestPath, manifest)
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