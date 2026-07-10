package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Meeting controls the auto-join meeting-notetaker capability. It follows the
// same opt-out (default-true) model as Telemetry: a node that has the audio +
// browser dependencies advertises the `meeting` tag and registers the
// MEETING_JOIN handler out of the box, and the operator may turn it off. This
// is the house convention for node capabilities — capabilities are config
// toggles, default opted-in, with a Control Center opt-out and a programmatic
// path (APPLY_DEVICE_CONFIG) that writes this same persisted value.
type Meeting struct {
	// MeetingEnabled gates the `meeting` capability. When true (and the node's
	// audio/browser deps are present) the node advertises the `meeting` tag and
	// registers the MEETING_JOIN handler. When false the node stays out of the
	// meeting queue regardless of deps. Defaults to true (opt-out) so meeting
	// nodes work out of the box; the Control Center settings pane discloses the
	// default-on behavior and offers the toggle.
	MeetingEnabled bool `yaml:"meeting_enabled" json:"meeting_enabled"`
}

const meetingFile = "meeting.yaml"

// DefaultMeeting returns a Meeting struct with the capability enabled.
// Default-on is intentional: it matches the house convention that capabilities
// are opt-out, so a dep-capable node joins meetings without extra setup.
func DefaultMeeting() *Meeting {
	return &Meeting{
		MeetingEnabled: true,
	}
}

// LoadMeeting reads meeting settings from the config directory.
// If the file doesn't exist, returns defaults (enabled).
// Partial files preserve defaults for absent keys (unmarshal into a
// pre-initialized struct), mirroring LoadTelemetry.
func LoadMeeting(configDir string) *Meeting {
	m := DefaultMeeting()

	data, err := os.ReadFile(filepath.Join(configDir, meetingFile))
	if err != nil {
		return m
	}

	// yaml.Unmarshal only overwrites keys present in the file, so an absent
	// key keeps its default (true) value.
	_ = yaml.Unmarshal(data, m)
	return m
}

// SaveMeeting writes meeting settings to the config directory.
// The Control Center settings pane calls this when the operator toggles the
// flag, and the APPLY_DEVICE_CONFIG handler calls it when the platform pushes
// an explicit value.
func SaveMeeting(configDir string, m *Meeting) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal meeting: %w", err)
	}

	return os.WriteFile(filepath.Join(configDir, meetingFile), data, 0644)
}
