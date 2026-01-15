// cmd/manifest.go
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
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
		Name string   `yaml:"name"`
		Tags []string `yaml:"tags"`
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