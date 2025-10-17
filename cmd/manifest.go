// cmd/manifest.go
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// --- Structs moved here from up.go to be shared ---
type Service struct {
	Name        string `yaml:"name"`
	ComposeFile string `yaml:"compose_file"`
}

type CitadelManifest struct {
	Node struct {
		Name string   `yaml:"name"`
		Tags []string `yaml:"tags"`
	} `yaml:"node"`
	Services []Service `yaml:"services"`
}

// findAndReadManifest is the new, smart way to load the config.
// It returns the manifest, the directory it was found in, and any error.
func findAndReadManifest() (*CitadelManifest, string, error) {
	// 1. Check current directory first
	localPath := "citadel.yaml"
	if _, err := os.Stat(localPath); err == nil {
		data, err := os.ReadFile(localPath)
		if err != nil {
			return nil, "", fmt.Errorf("could not read local manifest %s: %w", localPath, err)
		}
		var manifest CitadelManifest
		if err := yaml.Unmarshal(data, &manifest); err != nil {
			return nil, "", fmt.Errorf("could not parse local manifest %s: %w", localPath, err)
		}
		// Return current working directory as the base path
		wd, _ := os.Getwd()
		return &manifest, wd, nil
	}

	// 2. If not local, check global config
	globalConfigFile := "/etc/citadel/config.yaml"
	if _, err := os.Stat(globalConfigFile); os.IsNotExist(err) {
		return nil, "", fmt.Errorf("could not find citadel.yaml in the current directory, and no global config is set at %s. Have you run 'citadel init'?", globalConfigFile)
	}

	globalConfigData, err := os.ReadFile(globalConfigFile)
	if err != nil {
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

	// 3. Load manifest from the path specified in the global config
	manifestPath := filepath.Join(globalConf.NodeConfigDir, "citadel.yaml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, "", fmt.Errorf("could not read manifest from global path %s: %w", manifestPath, err)
	}

	var manifest CitadelManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, "", fmt.Errorf("could not parse manifest from global path %s: %w", manifestPath, err)
	}

	return &manifest, globalConf.NodeConfigDir, nil
}
