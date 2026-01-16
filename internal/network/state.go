// internal/network/state.go
// State directory management for tsnet network connections
package network

import (
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// GetStateDir returns the state directory path for network state.
// This is where tsnet stores WireGuard keys and connection state.
//
// The state directory is determined by:
// 1. First checking the global config file for node_config_dir setting
// 2. If not found, falling back to user home directory
//
// This ensures consistency when running with sudo - state follows the config.
func GetStateDir() string {
	// First, try to get state dir from global config (follows config location)
	if nodeDir := getNodeConfigDirFromGlobalConfig(); nodeDir != "" {
		return filepath.Join(nodeDir, "network")
	}

	// Fallback to user home directory
	return filepath.Join(getUserHomeDir(), "citadel-node", "network")
}

// getUserHomeDir returns the user's home directory
func getUserHomeDir() string {
	if runtime.GOOS == "windows" {
		baseDir := os.Getenv("USERPROFILE")
		if baseDir == "" {
			baseDir = os.Getenv("HOME")
		}
		return baseDir
	}

	baseDir, err := os.UserHomeDir()
	if err != nil {
		baseDir = os.Getenv("HOME")
	}
	return baseDir
}

// getNodeConfigDirFromGlobalConfig reads node_config_dir from global config
func getNodeConfigDirFromGlobalConfig() string {
	globalConfigFile := filepath.Join(getGlobalConfigDirForState(), "config.yaml")

	data, err := os.ReadFile(globalConfigFile)
	if err != nil {
		return ""
	}

	var config struct {
		NodeConfigDir string `yaml:"node_config_dir"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return ""
	}

	return config.NodeConfigDir
}

// getGlobalConfigDirForState returns the global config directory path
func getGlobalConfigDirForState() string {
	switch runtime.GOOS {
	case "darwin":
		return "/usr/local/etc/citadel"
	case "windows":
		return filepath.Join(os.Getenv("ProgramData"), "Citadel")
	default:
		return "/etc/citadel"
	}
}

// EnsureStateDir creates the state directory if it doesn't exist.
// Returns the state directory path.
func EnsureStateDir() (string, error) {
	stateDir := GetStateDir()
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return "", err
	}
	return stateDir, nil
}

// ClearState removes all network state, effectively logging out.
// This removes WireGuard keys and forces re-authentication on next connect.
func ClearState() error {
	stateDir := GetStateDir()
	return os.RemoveAll(stateDir)
}

// HasState returns true if network state exists (previously connected).
func HasState() bool {
	stateDir := GetStateDir()
	info, err := os.Stat(stateDir)
	if err != nil {
		return false
	}
	if !info.IsDir() {
		return false
	}

	// Check for tsnet state file
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// GetStatePath returns the full path for a state file.
func GetStatePath(filename string) string {
	return filepath.Join(GetStateDir(), filename)
}
