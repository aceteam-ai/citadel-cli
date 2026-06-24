package controlcenter

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// SettingsCallbacks holds the load/save hooks for the Settings page.
// They are wired from cmd so the page never imports platform/config-dir logic.
type SettingsCallbacks struct {
	// LoadTelemetry returns the current persisted telemetry setting.
	LoadTelemetry func() *config.Telemetry
	// SaveTelemetry persists an updated telemetry setting.
	SaveTelemetry func(*config.Telemetry) error

	// LoadKeepAwake returns the current persisted keep-awake setting.
	LoadKeepAwake func() *config.KeepAwake
	// SaveKeepAwake persists an updated keep-awake setting.
	SaveKeepAwake func(*config.KeepAwake) error
	// KeepAwakeState reports the live runtime state of the keep-awake monitor
	// (whether an assertion is currently held and the detected power source).
	// May be nil if the monitor is not running. The string is a short,
	// already-formatted label like "on (AC)" or "off (battery)".
	KeepAwakeState func() string
}

// connStatusProvider exposes the user-facing connection status. The ChatPage
// implements this; the transport detail (Redis) is intentionally hidden.
type connStatusProvider interface {
	ConnectionStatus() (endpoint string, state ConnState)
}

// SettingsPage implements the Page interface for the Settings tab.
//
// It manages the anonymous-telemetry opt-out (persisted via SettingsCallbacks)
// and surfaces a read-only view of the realtime connection status (which WSS
// endpoint the control/chat link uses and whether it is healthy).
type SettingsPage struct {
	app *tview.Application

	cb         SettingsCallbacks
	connStatus connStatusProvider

	// State
	telemetry *config.Telemetry
	keepAwake *config.KeepAwake

	// UI
	root *tview.Flex
	view *tview.TextView
}

// NewSettingsPage creates a Settings page. connStatus may be nil (status is
// then reported as unavailable). The telemetry setting is loaded lazily on
// first activation so it always reflects the latest persisted value.
func NewSettingsPage(cb SettingsCallbacks, connStatus connStatusProvider) *SettingsPage {
	return &SettingsPage{
		cb:         cb,
		connStatus: connStatus,
	}
}

// Name implements Page.
func (p *SettingsPage) Name() string { return "settings" }

// Title implements Page.
func (p *SettingsPage) Title() string { return "Settings" }

// Build implements Page.
func (p *SettingsPage) Build(app *tview.Application) tview.Primitive {
	p.app = app

	p.view = tview.NewTextView()
	p.view.SetDynamicColors(true)
	p.view.SetScrollable(true)
	p.view.SetBorder(true)
	p.view.SetTitle(" Settings ")
	p.view.SetTitleAlign(tview.AlignLeft)

	p.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(p.view, 0, 1, true)

	return p.root
}

// OnActivate implements Page. Reloads persisted settings and re-renders.
func (p *SettingsPage) OnActivate() {
	p.reloadTelemetry()
	p.reloadKeepAwake()
	p.render()
	if p.app != nil && p.view != nil {
		p.app.SetFocus(p.view)
	}
}

// OnDeactivate implements Page.
func (p *SettingsPage) OnDeactivate() {}

// HandleInput implements Page. Space/Enter/'t' toggles the telemetry opt-out;
// 'k' toggles the keep-awake-on-AC opt-in.
func (p *SettingsPage) HandleInput(event *tcell.EventKey) *tcell.EventKey {
	switch {
	case event.Key() == tcell.KeyRune && (event.Rune() == 'k' || event.Rune() == 'K'):
		p.toggleKeepAwake()
		return nil
	case event.Key() == tcell.KeyRune && (event.Rune() == ' ' || event.Rune() == 't' || event.Rune() == 'T'):
		p.toggleTelemetry()
		return nil
	case event.Key() == tcell.KeyEnter:
		p.toggleTelemetry()
		return nil
	}
	return event
}

// reloadTelemetry refreshes the in-memory telemetry setting from disk.
func (p *SettingsPage) reloadTelemetry() {
	if p.cb.LoadTelemetry != nil {
		p.telemetry = p.cb.LoadTelemetry()
	}
	if p.telemetry == nil {
		p.telemetry = config.DefaultTelemetry()
	}
}

// toggleTelemetry flips the opt-out setting and persists it.
func (p *SettingsPage) toggleTelemetry() {
	if p.telemetry == nil {
		p.reloadTelemetry()
	}

	next := &config.Telemetry{AnonTelemetryEnabled: !p.telemetry.AnonTelemetryEnabled}

	if p.cb.SaveTelemetry != nil {
		if err := p.cb.SaveTelemetry(next); err != nil {
			p.telemetry = next
			p.renderWithError(fmt.Sprintf("Failed to save: %v", err))
			return
		}
	}
	p.telemetry = next
	p.render()
}

// reloadKeepAwake refreshes the in-memory keep-awake setting from disk.
func (p *SettingsPage) reloadKeepAwake() {
	if p.cb.LoadKeepAwake != nil {
		p.keepAwake = p.cb.LoadKeepAwake()
	}
	if p.keepAwake == nil {
		p.keepAwake = config.DefaultKeepAwake()
	}
}

// toggleKeepAwake flips the keep-awake-on-AC opt-in and persists it.
func (p *SettingsPage) toggleKeepAwake() {
	if p.keepAwake == nil {
		p.reloadKeepAwake()
	}

	next := &config.KeepAwake{KeepAwakeOnAC: !p.keepAwake.KeepAwakeOnAC}

	if p.cb.SaveKeepAwake != nil {
		if err := p.cb.SaveKeepAwake(next); err != nil {
			p.keepAwake = next
			p.renderWithError(fmt.Sprintf("Failed to save: %v", err))
			return
		}
	}
	p.keepAwake = next
	p.render()
}

// render redraws the settings view from current state.
func (p *SettingsPage) render() {
	p.renderWithError("")
}

func (p *SettingsPage) renderWithError(errMsg string) {
	if p.view == nil {
		return
	}

	enabled := p.telemetry != nil && p.telemetry.AnonTelemetryEnabled

	var sb strings.Builder

	// -- Anonymous data collection --
	sb.WriteString("\n [yellow::b]Anonymous Data Collection[-:-:-]\n\n")
	if enabled {
		sb.WriteString("   Status:  [green::b]ON[-:-:-]  [gray](collecting)[-]\n\n")
	} else {
		sb.WriteString("   Status:  [red::b]OFF[-:-:-]  [gray](opted out)[-]\n\n")
	}
	sb.WriteString("   [white]What is collected:[-] anonymous debug and activity events\n")
	sb.WriteString("   (errors, feature usage). No file contents, no message bodies,\n")
	sb.WriteString("   no personal data.\n\n")
	sb.WriteString("   [white]Why:[-] it helps us debug problems remotely and improve\n")
	sb.WriteString("   Citadel. Opting out stops [white::b]all[-:-:-] anonymous collection.\n\n")
	sb.WriteString("   [yellow::b]Space[-:-:-]/[yellow::b]Enter[-:-:-] toggle collection\n")

	// -- Keep-awake on AC --
	keepEnabled := p.keepAwake != nil && p.keepAwake.KeepAwakeOnAC
	sb.WriteString("\n [yellow::b]Keep Awake on AC[-:-:-]\n\n")
	if keepEnabled {
		sb.WriteString("   Status:  [green::b]ON[-:-:-]  [gray](opted in)[-]\n")
	} else {
		sb.WriteString("   Status:  [red::b]OFF[-:-:-]  [gray](default)[-]\n")
	}
	if p.cb.KeepAwakeState != nil {
		if live := p.cb.KeepAwakeState(); live != "" {
			sb.WriteString(fmt.Sprintf("   Keep-awake:  [white]%s[-]\n", live))
		}
	}
	sb.WriteString("\n   [white]What it does:[-] holds a system idle-sleep assertion while\n")
	sb.WriteString("   this node is plugged in, so the laptop stays reachable on the\n")
	sb.WriteString("   mesh. Released on battery and on exit. Display may still sleep.\n\n")
	sb.WriteString("   [yellow::b]k[-:-:-] toggle keep-awake\n")

	// -- Connection status (read-only) --
	sb.WriteString("\n [yellow::b]Connection[-:-:-]\n\n")
	endpoint, state := p.connectionStatus()
	if endpoint == "" {
		endpoint = "[gray]not configured[-]"
	}
	sb.WriteString(fmt.Sprintf("   Endpoint:  [white]%s[-]\n", endpoint))
	sb.WriteString(fmt.Sprintf("   Health:    %s\n", connStateLabel(state)))

	// -- Error line --
	if errMsg != "" {
		sb.WriteString(fmt.Sprintf("\n   [red]%s[-]\n", tview.Escape(errMsg)))
	}

	p.view.SetText(sb.String())
}

// connectionStatus returns the WSS endpoint + state, best-effort.
func (p *SettingsPage) connectionStatus() (string, ConnState) {
	if p.connStatus == nil {
		return "", ConnDisconnected
	}
	return p.connStatus.ConnectionStatus()
}

// connStateLabel renders a colored health label for a connection state.
func connStateLabel(state ConnState) string {
	switch state {
	case ConnConnected:
		return "[green::b]● connected[-:-:-]"
	case ConnConnecting:
		return "[yellow::b]◐ connecting[-:-:-]"
	default:
		return "[red::b]○ disconnected[-:-:-]"
	}
}

// wssEndpoint derives the user-facing WSS endpoint host from the API base URL.
// It surfaces only the scheme+host (e.g. "wss://aceteam.ai") and deliberately
// omits the backend path so the transport (Redis) is never exposed.
func wssEndpoint(apiBaseURL string) string {
	if apiBaseURL == "" {
		return ""
	}
	u, err := url.Parse(apiBaseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	return u.Scheme + "://" + u.Host
}
