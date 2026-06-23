package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Telemetry controls anonymous debug/activity event collection.
//
// This follows an opt-out model: collection is enabled by default and the
// user may disable it. The single key is shared with the telemetry emission
// path (issue #294) so the Settings toggle and the emitter agree on whether
// collection is permitted.
type Telemetry struct {
	// AnonTelemetryEnabled gates all anonymous telemetry collection. When
	// false, no anonymous debug/activity events are collected or sent.
	AnonTelemetryEnabled bool `yaml:"anon_telemetry_enabled" json:"anon_telemetry_enabled"`
}

const telemetryFile = "telemetry.yaml"

// DefaultTelemetry returns a Telemetry struct with collection enabled (opt-out).
func DefaultTelemetry() *Telemetry {
	return &Telemetry{
		AnonTelemetryEnabled: true,
	}
}

// LoadTelemetry reads telemetry settings from the config directory.
// If the file doesn't exist, returns the opt-out default (enabled).
// Absent keys preserve their default value (unmarshal into a pre-initialized
// struct), so a partial file never silently disables collection.
func LoadTelemetry(configDir string) *Telemetry {
	t := DefaultTelemetry()

	data, err := os.ReadFile(filepath.Join(configDir, telemetryFile))
	if err != nil {
		return t
	}

	_ = yaml.Unmarshal(data, t)
	return t
}

// SaveTelemetry writes telemetry settings to the config directory.
func SaveTelemetry(configDir string, t *Telemetry) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal telemetry: %w", err)
	}

	return os.WriteFile(filepath.Join(configDir, telemetryFile), data, 0644)
}
