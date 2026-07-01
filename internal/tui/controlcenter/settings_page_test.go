package controlcenter

import (
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

var errTestSave = errors.New("save failed")

// fakeConnStatus is a stub connStatusProvider for tests.
type fakeConnStatus struct {
	endpoint string
	state    ConnState
}

func (f fakeConnStatus) ConnectionStatus() (string, ConnState) {
	return f.endpoint, f.state
}

// newTestSettings wires a SettingsPage to an in-memory telemetry store.
func newTestSettings() (*SettingsPage, *config.Telemetry) {
	store := config.DefaultTelemetry()
	cb := SettingsCallbacks{
		LoadTelemetry: func() *config.Telemetry {
			// Return a copy so the page can't mutate the store directly.
			cp := *store
			return &cp
		},
		SaveTelemetry: func(t *config.Telemetry) error {
			store.AnonTelemetryEnabled = t.AnonTelemetryEnabled
			return nil
		},
	}
	p := NewSettingsPage(cb, fakeConnStatus{endpoint: "wss://aceteam.ai", state: ConnConnected})
	return p, store
}

func TestSettingsToggleTelemetry_PersistsOptOut(t *testing.T) {
	p, store := newTestSettings()
	p.reloadTelemetry()

	if !p.telemetry.AnonTelemetryEnabled {
		t.Fatalf("expected default enabled, got %+v", p.telemetry)
	}

	// First toggle: opt out. Must persist false to the store.
	p.toggleTelemetry()
	if p.telemetry.AnonTelemetryEnabled {
		t.Error("in-memory state should be disabled after toggle")
	}
	if store.AnonTelemetryEnabled {
		t.Error("persisted store should be disabled after toggle (opt-out not saved)")
	}

	// Second toggle: opt back in.
	p.toggleTelemetry()
	if !p.telemetry.AnonTelemetryEnabled {
		t.Error("in-memory state should be enabled after second toggle")
	}
	if !store.AnonTelemetryEnabled {
		t.Error("persisted store should be enabled after second toggle")
	}
}

func TestSettingsToggle_SaveErrorKeepsState(t *testing.T) {
	saved := false
	cb := SettingsCallbacks{
		LoadTelemetry: config.DefaultTelemetry,
		SaveTelemetry: func(*config.Telemetry) error {
			saved = true
			return errTestSave
		},
	}
	p := NewSettingsPage(cb, nil)
	p.Build(nil) // initialize view so renderWithError doesn't no-op silently
	p.reloadTelemetry()

	p.toggleTelemetry()
	if !saved {
		t.Error("SaveTelemetry should have been invoked")
	}
	// Even on save error, the in-memory state reflects the attempted change so
	// the UI stays consistent with what the user pressed.
	if p.telemetry.AnonTelemetryEnabled {
		t.Error("in-memory state should reflect attempted toggle even on save error")
	}
}

func TestSettingsToggleKeepAwake_PersistsOptIn(t *testing.T) {
	store := config.DefaultKeepAwake()
	cb := SettingsCallbacks{
		LoadKeepAwake: func() *config.KeepAwake {
			cp := *store
			return &cp
		},
		SaveKeepAwake: func(k *config.KeepAwake) error {
			store.KeepAwakeOnAC = k.KeepAwakeOnAC
			return nil
		},
	}
	p := NewSettingsPage(cb, nil)
	p.reloadKeepAwake()

	if p.keepAwake.KeepAwakeOnAC {
		t.Fatalf("expected default disabled (opt-in), got %+v", p.keepAwake)
	}

	// Opt in.
	p.toggleKeepAwake()
	if !p.keepAwake.KeepAwakeOnAC {
		t.Error("in-memory state should be enabled after toggle")
	}
	if !store.KeepAwakeOnAC {
		t.Error("persisted store should be enabled after opt-in")
	}

	// Opt back out.
	p.toggleKeepAwake()
	if p.keepAwake.KeepAwakeOnAC {
		t.Error("in-memory state should be disabled after second toggle")
	}
	if store.KeepAwakeOnAC {
		t.Error("persisted store should be disabled after opt-out")
	}
}

func TestSettingsToggleMouse_PersistsAndAppliesLive(t *testing.T) {
	store := config.DefaultMouse() // Enabled: true by default
	var liveState []bool
	cb := SettingsCallbacks{
		LoadMouse: func() *config.Mouse {
			cp := *store
			return &cp
		},
		SaveMouse: func(m *config.Mouse) error {
			store.Enabled = m.Enabled
			return nil
		},
		SetMouseEnabled: func(enabled bool) {
			liveState = append(liveState, enabled)
		},
	}
	p := NewSettingsPage(cb, nil)
	p.reloadMouse()

	if !p.mouse.Enabled {
		t.Fatalf("expected default enabled, got %+v", p.mouse)
	}

	// Toggle off: persists false and applies live immediately.
	p.toggleMouse()
	if p.mouse.Enabled {
		t.Error("in-memory state should be disabled after toggle")
	}
	if store.Enabled {
		t.Error("persisted store should be disabled after toggle")
	}
	if len(liveState) != 1 || liveState[0] != false {
		t.Errorf("SetMouseEnabled should have been called once with false, got %v", liveState)
	}

	// Toggle back on.
	p.toggleMouse()
	if !p.mouse.Enabled {
		t.Error("in-memory state should be enabled after second toggle")
	}
	if !store.Enabled {
		t.Error("persisted store should be enabled after second toggle")
	}
	if len(liveState) != 2 || liveState[1] != true {
		t.Errorf("SetMouseEnabled should have been called with true, got %v", liveState)
	}
}

func TestSettingsToggleMouse_SaveErrorStillAppliesLive(t *testing.T) {
	var liveState []bool
	cb := SettingsCallbacks{
		LoadMouse: config.DefaultMouse,
		SaveMouse: func(*config.Mouse) error {
			return errTestSave
		},
		SetMouseEnabled: func(enabled bool) {
			liveState = append(liveState, enabled)
		},
	}
	p := NewSettingsPage(cb, nil)
	p.Build(nil)
	p.reloadMouse()

	p.toggleMouse()
	// Live apply must happen even when persistence fails, so the UX is not stuck.
	if len(liveState) != 1 || liveState[0] != false {
		t.Errorf("SetMouseEnabled should apply live even on save error, got %v", liveState)
	}
	if p.mouse.Enabled {
		t.Error("in-memory state should reflect attempted toggle even on save error")
	}
}

func TestSettingsToggleFullscreen_Persists(t *testing.T) {
	store := config.DefaultRendering() // Fullscreen: true by default
	var seam []bool
	cb := SettingsCallbacks{
		LoadRendering: func() *config.Rendering {
			cp := *store
			return &cp
		},
		SaveRendering: func(r *config.Rendering) error {
			store.Fullscreen = r.Fullscreen
			return nil
		},
		SetFullscreenEnabled: func(enabled bool) {
			seam = append(seam, enabled)
		},
	}
	p := NewSettingsPage(cb, nil)
	p.reloadRendering()

	if !p.rendering.Fullscreen {
		t.Fatalf("expected default fullscreen enabled, got %+v", p.rendering)
	}

	// Toggle off: persists false. The seam is invoked for any future live-apply
	// consumer, but the real apply is deferred to next launch.
	p.toggleFullscreen()
	if p.rendering.Fullscreen {
		t.Error("in-memory state should be disabled after toggle")
	}
	if store.Fullscreen {
		t.Error("persisted store should be disabled after toggle")
	}
	if len(seam) != 1 || seam[0] != false {
		t.Errorf("SetFullscreenEnabled seam should have been called once with false, got %v", seam)
	}

	// Toggle back on.
	p.toggleFullscreen()
	if !p.rendering.Fullscreen {
		t.Error("in-memory state should be enabled after second toggle")
	}
	if !store.Fullscreen {
		t.Error("persisted store should be enabled after second toggle")
	}
	if len(seam) != 2 || seam[1] != true {
		t.Errorf("SetFullscreenEnabled seam should have been called with true, got %v", seam)
	}
}

func TestSettingsToggleFullscreen_SaveErrorKeepsState(t *testing.T) {
	cb := SettingsCallbacks{
		LoadRendering: config.DefaultRendering,
		SaveRendering: func(*config.Rendering) error {
			return errTestSave
		},
	}
	p := NewSettingsPage(cb, nil)
	p.Build(nil)
	p.reloadRendering()

	p.toggleFullscreen()
	// Even on save error, in-memory state reflects the attempted change so the UI
	// stays consistent with what the user pressed.
	if p.rendering.Fullscreen {
		t.Error("in-memory state should reflect attempted toggle even on save error")
	}
}

// The Mouse & Rendering headline section must render both checkboxes with the
// mock's exact copy so the panel matches the approved design.
func TestSettingsRender_MouseAndRenderingCopy(t *testing.T) {
	cb := SettingsCallbacks{
		LoadTelemetry: config.DefaultTelemetry,
		LoadKeepAwake: config.DefaultKeepAwake,
		LoadMouse:     config.DefaultMouse,
		LoadRendering: config.DefaultRendering,
	}
	p := NewSettingsPage(cb, fakeConnStatus{endpoint: "wss://aceteam.ai", state: ConnConnected})
	p.Build(nil)
	p.OnActivate()

	got := p.view.GetText(true) // stripped of color tags

	wantFragments := []string{
		"Mouse & Rendering",
		"Mouse control",
		"Click tabs, peers, and Send instead of memorizing keys.",
		"Tradeoff: your terminal's drag-to-copy stops working while",
		"this is on. To copy anyway, hold:",
		"• Shift        (most terminals)",
		"• Fn           (macOS Terminal.app)",
		"• Option        (iTerm2)",
		"Fullscreen rendering",
		"Flicker-free, app-like. Off = output goes to normal",
		"scrollback (easier to scroll + copy long history).",
	}
	for _, frag := range wantFragments {
		if !strings.Contains(got, frag) {
			t.Errorf("rendered settings missing expected copy fragment:\n  %q\nfull render:\n%s", frag, got)
		}
	}
}

// Checkbox glyphs must reflect each setting's on/off state so the panel tells
// the truth about what is enabled.
func TestSettingsRender_CheckboxReflectsState(t *testing.T) {
	cb := SettingsCallbacks{
		LoadTelemetry: config.DefaultTelemetry,
		LoadKeepAwake: config.DefaultKeepAwake,
		LoadMouse:     func() *config.Mouse { return &config.Mouse{Enabled: true} },
		LoadRendering: func() *config.Rendering { return &config.Rendering{Fullscreen: false} },
	}
	p := NewSettingsPage(cb, nil)
	p.Build(nil)
	p.OnActivate()

	got := p.view.GetText(true)
	// Mouse on -> checked; Fullscreen off -> unchecked. Both rows must be present.
	if !strings.Contains(got, "[✓] Mouse control") {
		t.Errorf("expected checked Mouse control row, got:\n%s", got)
	}
	if !strings.Contains(got, "[ ] Fullscreen rendering") {
		t.Errorf("expected unchecked Fullscreen rendering row, got:\n%s", got)
	}
}

func TestWSSEndpoint_HidesRedisTransport(t *testing.T) {
	cases := map[string]string{
		"https://aceteam.ai":          "wss://aceteam.ai",
		"http://localhost:3000":       "ws://localhost:3000",
		"https://staging.aceteam.ai/": "wss://staging.aceteam.ai",
		"wss://already.example":       "wss://already.example",
		"":                            "",
		"://bad":                      "",
	}
	for in, want := range cases {
		got := wssEndpoint(in)
		if got != want {
			t.Errorf("wssEndpoint(%q) = %q, want %q", in, got, want)
		}
		// The user-facing endpoint must never reveal the Redis transport path.
		if strings.Contains(strings.ToLower(got), "redis") {
			t.Errorf("wssEndpoint(%q) = %q leaks redis transport detail", in, got)
		}
	}
}
