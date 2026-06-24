package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// KeepAwake controls AC-aware sleep inhibition. It follows the same opt-in
// (default-false) model the issue mandates: a laptop running `citadel work`
// must never have its power policy overridden without explicit consent, so
// this defaults off and the operator turns it on from the settings pane (#295)
// or by editing the config file.
type KeepAwake struct {
	// KeepAwakeOnAC gates system idle-sleep inhibition while the node is on AC
	// power. When true and the machine is plugged in, `citadel work` holds a
	// process-scoped OS power assertion so the node stays reachable on the mesh.
	// On battery (or when false) no assertion is held, so an unplugged laptop is
	// never kept awake. Defaults to false (opt-in).
	KeepAwakeOnAC bool `yaml:"keep_awake_on_ac" json:"keep_awake_on_ac"`
}

const keepAwakeFile = "keepawake.yaml"

// DefaultKeepAwake returns a KeepAwake struct with inhibition disabled.
// Default-off is intentional: keeping a machine awake is a power-policy change
// the operator must opt into.
func DefaultKeepAwake() *KeepAwake {
	return &KeepAwake{
		KeepAwakeOnAC: false,
	}
}

// LoadKeepAwake reads keep-awake settings from the config directory.
// If the file doesn't exist, returns defaults (disabled).
// Partial files preserve defaults for absent keys (unmarshal into a
// pre-initialized struct), mirroring LoadTelemetry.
func LoadKeepAwake(configDir string) *KeepAwake {
	k := DefaultKeepAwake()

	data, err := os.ReadFile(filepath.Join(configDir, keepAwakeFile))
	if err != nil {
		return k
	}

	// yaml.Unmarshal only overwrites keys present in the file, so an absent
	// key keeps its default (false) value.
	_ = yaml.Unmarshal(data, k)
	return k
}

// SaveKeepAwake writes keep-awake settings to the config directory.
// The settings pane (#295) calls this when the operator toggles the flag.
func SaveKeepAwake(configDir string, k *KeepAwake) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(k)
	if err != nil {
		return fmt.Errorf("marshal keepawake: %w", err)
	}

	return os.WriteFile(filepath.Join(configDir, keepAwakeFile), data, 0644)
}
