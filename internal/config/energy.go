package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Energy controls the per-request energy receipt sampler (aceteam#6635). Unlike
// the capability opt-OUT toggles (meeting, telemetry), energy sampling is
// default-OFF: it adds an nvidia-smi power probe per footprint tick and three
// extra CSV columns, so a node opts IN explicitly. The operator enables it here
// (energy.yaml) or via the CITADEL_ENERGY_SAMPLING env var; the platform can push
// the same persisted value through APPLY_DEVICE_CONFIG (DeviceConfig.EnergySampling).
type Energy struct {
	// SamplingEnabled turns the footprint energy estimate on. When false (the
	// default) the footprint sampler runs exactly as before the energy feature
	// existed: no power probe, no power_w / energy_wh / power_source columns. When
	// true the node-level footprint row carries a per-interval watt-hours estimate
	// labeled measured (a real GPU power sensor) or estimated (a modeled figure).
	SamplingEnabled bool `yaml:"sampling_enabled" json:"sampling_enabled"`
}

const energyFile = "energy.yaml"

// DefaultEnergy returns Energy with sampling disabled: the opt-in default. Energy
// sampling is off until an operator or the platform turns it on.
func DefaultEnergy() *Energy {
	return &Energy{SamplingEnabled: false}
}

// LoadEnergy reads energy settings from the config directory. A missing file
// returns defaults (disabled). Partial files preserve defaults for absent keys
// (unmarshal into a pre-initialized struct), mirroring LoadTelemetry.
func LoadEnergy(configDir string) *Energy {
	e := DefaultEnergy()

	data, err := os.ReadFile(filepath.Join(configDir, energyFile))
	if err != nil {
		return e
	}

	// yaml.Unmarshal only overwrites keys present in the file, so an absent key
	// keeps its default (false) value.
	_ = yaml.Unmarshal(data, e)
	return e
}

// SaveEnergy writes energy settings to the config directory. The
// APPLY_DEVICE_CONFIG handler calls this when the platform pushes the toggle.
func SaveEnergy(configDir string, e *Energy) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal energy: %w", err)
	}

	return os.WriteFile(filepath.Join(configDir, energyFile), data, 0644)
}
