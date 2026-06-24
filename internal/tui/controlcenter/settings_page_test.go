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
