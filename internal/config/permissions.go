// Package config provides configuration types for Citadel node settings.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Permissions controls which capabilities are exposed through the HTTPS gateway.
// All fields default to true (opt-out model).
type Permissions struct {
	Console   bool `yaml:"console" json:"console"`     // Terminal WebSocket access
	Desktop   bool `yaml:"desktop" json:"desktop"`     // VNC, screenshots, actions
	Files     bool `yaml:"files" json:"files"`         // File browser API
	Services  bool `yaml:"services" json:"services"`   // Service list/management
	SSH       bool `yaml:"ssh" json:"ssh"`             // SSH authorized_keys sync
	Provision bool `yaml:"provision" json:"provision"` // Container provisioning API
}

const permissionsFile = "permissions.yaml"

// DefaultPermissions returns a Permissions struct with all capabilities enabled.
func DefaultPermissions() *Permissions {
	return &Permissions{
		Console:   true,
		Desktop:   true,
		Files:     true,
		Services:  true,
		SSH:       true,
		Provision: true,
	}
}

// LoadPermissions reads permissions from the config directory.
// If the file doesn't exist, returns all-true defaults.
// Partial files preserve defaults for absent keys (unmarshal into pre-initialized struct).
func LoadPermissions(configDir string) *Permissions {
	p := DefaultPermissions()

	data, err := os.ReadFile(filepath.Join(configDir, permissionsFile))
	if err != nil {
		return p
	}

	// yaml.Unmarshal only overwrites keys present in the file,
	// so absent keys keep their default (true) value.
	_ = yaml.Unmarshal(data, p)
	return p
}

// SavePermissions writes permissions to the config directory.
func SavePermissions(configDir string, p *Permissions) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal permissions: %w", err)
	}

	return os.WriteFile(filepath.Join(configDir, permissionsFile), data, 0644)
}
