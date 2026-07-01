package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Rendering controls how the control-center TUI draws to the terminal. Like
// Mouse (opt-out, default-on), fullscreen rendering is the experience the user
// is expected to want — a flicker-free, app-like screen — so it defaults ON. It
// is a per-user preference persisted alongside the other config files.
//
// When Fullscreen is true the TUI uses the terminal's alternate screen buffer
// (the whole viewport is repainted in place, no flicker, and the screen is
// restored on exit). When false, output goes to the normal scrollback buffer,
// which is easier to scroll and copy long history from but does not get the
// app-like repaint.
//
// Consuming this preference at screen-creation time lives in the control
// center's Run() path; the Settings pane only persists the choice. See the
// SetFullscreenEnabled seam in SettingsCallbacks for the live/launch-time apply.
type Rendering struct {
	// Fullscreen gates use of the terminal's alternate screen buffer. When true
	// (the default), the TUI renders flicker-free, app-like, and restores the
	// prior screen on exit. When false, output goes to normal scrollback so long
	// history is easier to scroll and copy.
	Fullscreen bool `yaml:"fullscreen" json:"fullscreen"`
}

const renderingFile = "rendering.yaml"

// DefaultRendering returns a Rendering struct with fullscreen enabled.
// Default-on is intentional: the flicker-free, app-like screen is the expected
// experience, and scrollback mode is an explicit opt-out for users who prefer to
// scroll/copy long history from their terminal's native buffer.
func DefaultRendering() *Rendering {
	return &Rendering{
		Fullscreen: true,
	}
}

// LoadRendering reads rendering settings from the config directory. If the file
// doesn't exist, returns defaults (fullscreen). Partial files preserve defaults
// for absent keys (unmarshal into a pre-initialized struct), mirroring
// LoadMouse.
func LoadRendering(configDir string) *Rendering {
	r := DefaultRendering()

	data, err := os.ReadFile(filepath.Join(configDir, renderingFile))
	if err != nil {
		return r
	}

	// yaml.Unmarshal only overwrites keys present in the file, so an absent key
	// keeps its default (true) value.
	_ = yaml.Unmarshal(data, r)
	return r
}

// SaveRendering writes rendering settings to the config directory. The Settings
// pane calls this when the operator toggles fullscreen rendering.
func SaveRendering(configDir string, r *Rendering) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal rendering: %w", err)
	}

	return os.WriteFile(filepath.Join(configDir, renderingFile), data, 0644)
}
