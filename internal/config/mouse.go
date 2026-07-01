package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Mouse controls whether the control-center TUI opts into terminal mouse
// reporting. Unlike KeepAwake (opt-in, default-off), mouse control is the
// feature the user is expected to want — clicking tabs, peers, and Send instead
// of memorizing keybindings — so it defaults ON. It is a per-user preference
// persisted alongside the other config files.
//
// The tradeoff mouse reporting imposes is that the terminal's native
// drag-to-copy stops working while it is on (the user must hold Shift / Fn /
// Option to bypass, depending on the terminal). Users who prefer native
// selection can disable it from the Settings pane or with the --no-mouse flag.
type Mouse struct {
	// Enabled gates terminal mouse reporting in the control center. When true
	// (the default), clicks/scroll drive the TUI. When false, the app never
	// enables mouse capture, preserving native terminal drag-to-copy.
	Enabled bool `yaml:"enabled" json:"enabled"`
}

const mouseFile = "mouse.yaml"

// DefaultMouse returns a Mouse struct with mouse control enabled. Default-on is
// intentional: the click-to-drive UX is the feature, and keyboard navigation is
// fully preserved either way.
func DefaultMouse() *Mouse {
	return &Mouse{
		Enabled: true,
	}
}

// LoadMouse reads mouse settings from the config directory. If the file doesn't
// exist, returns defaults (enabled). Partial files preserve defaults for absent
// keys (unmarshal into a pre-initialized struct), mirroring LoadKeepAwake.
func LoadMouse(configDir string) *Mouse {
	m := DefaultMouse()

	data, err := os.ReadFile(filepath.Join(configDir, mouseFile))
	if err != nil {
		return m
	}

	// yaml.Unmarshal only overwrites keys present in the file, so an absent key
	// keeps its default (true) value.
	_ = yaml.Unmarshal(data, m)
	return m
}

// SaveMouse writes mouse settings to the config directory. The Settings pane
// calls this when the operator toggles mouse control.
func SaveMouse(configDir string, m *Mouse) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal mouse: %w", err)
	}

	return os.WriteFile(filepath.Join(configDir, mouseFile), data, 0644)
}
