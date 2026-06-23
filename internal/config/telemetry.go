package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Telemetry controls anonymous activity-event streaming used for remote
// debugging. It follows the same opt-out (default-true) model as Permissions:
// the node streams its operational activity feed to the AceTeam control plane
// so operators and the platform can debug node issues remotely, and the
// operator may turn it off.
type Telemetry struct {
	// AnonTelemetryEnabled gates ALL activity-event emission. When false, no
	// activity events leave the node. Defaults to true (opt-out) so that nodes
	// are debuggable out of the box; the settings pane (#295) and `citadel
	// init` are responsible for disclosing this default-on behavior to the
	// operator and offering the toggle.
	AnonTelemetryEnabled bool `yaml:"anon_telemetry_enabled" json:"anon_telemetry_enabled"`
}

const telemetryFile = "telemetry.yaml"

// DefaultTelemetry returns a Telemetry struct with anonymous telemetry enabled.
// Default-on is intentional: the activity feed is anonymous (node/debug context
// only, no user PII) and remote visibility into the activity log is the primary
// mechanism for debugging field nodes we cannot SSH into.
func DefaultTelemetry() *Telemetry {
	return &Telemetry{
		AnonTelemetryEnabled: true,
	}
}

// LoadTelemetry reads telemetry settings from the config directory.
// If the file doesn't exist, returns defaults (enabled).
// Partial files preserve defaults for absent keys (unmarshal into a
// pre-initialized struct), mirroring LoadPermissions.
func LoadTelemetry(configDir string) *Telemetry {
	t := DefaultTelemetry()

	data, err := os.ReadFile(filepath.Join(configDir, telemetryFile))
	if err != nil {
		return t
	}

	// yaml.Unmarshal only overwrites keys present in the file, so an absent
	// key keeps its default (true) value.
	_ = yaml.Unmarshal(data, t)
	return t
}

// SaveTelemetry writes telemetry settings to the config directory.
// The settings pane (#295) calls this when the operator toggles the flag.
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
