package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// DefaultServe controls the "default-serve" appliance-mode reconcile
// (citadel-cli#628): a `citadel work` startup check that, on a truly blank
// GPU node, auto-serves a VRAM-sized model exactly once, ever. Like Energy
// (aceteam#6635) and every other toggle that changes eviction/refusal/deploy
// behavior on a node that has never seen it, this is default-OFF: a node
// with no opt-in is byte-identical to one without this feature at all. The
// operator enables it here (default-serve.yaml), via the CITADEL_DEFAULT_SERVE
// env var, or via the manifest's default_serve key; the platform can push the
// same persisted value through APPLY_DEVICE_CONFIG
// (DeviceConfig.DefaultServe). See cmd/default_serve.go's resolveDefaultServe
// for the exact precedence among all three sources.
type DefaultServe struct {
	// Enabled turns the default-serve reconcile on. When false (the default)
	// `citadel work` startup never runs the blank-node check at all.
	Enabled bool `yaml:"enabled" json:"enabled"`
}

const defaultServeFile = "default-serve.yaml"

// DefaultDefaultServe returns DefaultServe with the reconcile disabled: the
// opt-in default.
func DefaultDefaultServe() *DefaultServe {
	return &DefaultServe{Enabled: false}
}

// LoadDefaultServe reads the default-serve toggle from the config directory.
// A missing file returns defaults (disabled). Partial files preserve
// defaults for absent keys (unmarshal into a pre-initialized struct),
// mirroring LoadEnergy/LoadTelemetry.
func LoadDefaultServe(configDir string) *DefaultServe {
	d := DefaultDefaultServe()

	data, err := os.ReadFile(filepath.Join(configDir, defaultServeFile))
	if err != nil {
		return d
	}

	// yaml.Unmarshal only overwrites keys present in the file, so an absent
	// key keeps its default (false) value.
	_ = yaml.Unmarshal(data, d)
	return d
}

// SaveDefaultServe writes the default-serve toggle to the config directory.
// The APPLY_DEVICE_CONFIG handler calls this when the platform pushes the
// toggle.
func SaveDefaultServe(configDir string, d *DefaultServe) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(d)
	if err != nil {
		return fmt.Errorf("marshal default-serve config: %w", err)
	}

	return os.WriteFile(filepath.Join(configDir, defaultServeFile), data, 0644)
}
