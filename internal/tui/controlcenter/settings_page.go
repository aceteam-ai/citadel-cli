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

	// LoadMouse returns the current persisted mouse-control setting.
	LoadMouse func() *config.Mouse
	// SaveMouse persists an updated mouse-control setting.
	SaveMouse func(*config.Mouse) error
	// SetMouseEnabled applies mouse capture live on the running app (no restart),
	// mirroring tview's app.EnableMouse. May be nil in contexts without an app.
	SetMouseEnabled func(bool)

	// LoadRendering returns the current persisted fullscreen-rendering setting.
	LoadRendering func() *config.Rendering
	// SaveRendering persists an updated fullscreen-rendering setting.
	SaveRendering func(*config.Rendering) error
	// SetFullscreenEnabled is the injection seam for applying the fullscreen
	// preference on the running app. Unlike mouse capture, tview cannot swap the
	// terminal's alternate-screen mode mid-run, so today this is a no-op seam that
	// a launch-time consumer in the control center's Run() path will fill (the
	// screen path is owned by controlcenter.go). May be nil.
	SetFullscreenEnabled func(bool)
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
	mouse     *config.Mouse
	rendering *config.Rendering

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
	p.reloadMouse()
	p.reloadRendering()
	p.render()
	if p.app != nil && p.view != nil {
		p.app.SetFocus(p.view)
	}
}

// OnDeactivate implements Page.
func (p *SettingsPage) OnDeactivate() {}

// HandleInput implements Page. 'm' toggles mouse control; 'f' toggles fullscreen
// rendering (the two Mouse & Rendering checkboxes); 'k' toggles keep-awake-on-AC;
// Space/Enter/'t' toggles the telemetry opt-out.
func (p *SettingsPage) HandleInput(event *tcell.EventKey) *tcell.EventKey {
	switch {
	case event.Key() == tcell.KeyRune && (event.Rune() == 'm' || event.Rune() == 'M'):
		p.toggleMouse()
		return nil
	case event.Key() == tcell.KeyRune && (event.Rune() == 'f' || event.Rune() == 'F'):
		p.toggleFullscreen()
		return nil
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

// reloadMouse refreshes the in-memory mouse setting from disk.
func (p *SettingsPage) reloadMouse() {
	if p.cb.LoadMouse != nil {
		p.mouse = p.cb.LoadMouse()
	}
	if p.mouse == nil {
		p.mouse = config.DefaultMouse()
	}
}

// toggleMouse flips mouse control, applies it live on the running app, and
// persists it. The live apply (SetMouseEnabled) takes effect immediately with no
// restart — strictly better than a setting that only applies next launch.
func (p *SettingsPage) toggleMouse() {
	if p.mouse == nil {
		p.reloadMouse()
	}

	next := &config.Mouse{Enabled: !p.mouse.Enabled}

	// Apply live first so the change is visible immediately even if persistence
	// fails; the error line still surfaces a save failure.
	if p.cb.SetMouseEnabled != nil {
		p.cb.SetMouseEnabled(next.Enabled)
	}

	if p.cb.SaveMouse != nil {
		if err := p.cb.SaveMouse(next); err != nil {
			p.mouse = next
			p.renderWithError(fmt.Sprintf("Failed to save: %v", err))
			return
		}
	}
	p.mouse = next
	p.render()
}

// reloadRendering refreshes the in-memory fullscreen-rendering setting from disk.
func (p *SettingsPage) reloadRendering() {
	if p.cb.LoadRendering != nil {
		p.rendering = p.cb.LoadRendering()
	}
	if p.rendering == nil {
		p.rendering = config.DefaultRendering()
	}
}

// toggleFullscreen flips fullscreen rendering and persists it. tview cannot swap
// the terminal's alternate-screen mode mid-run, so this persists the choice and
// it takes effect on the next launch: the control center's Run() reads the saved
// Rendering preference and stages TCELL_ALTSCREEN before creating the screen (see
// applyFullscreenRendering). The inline hint tells the user to restart to apply.
// SetFullscreenEnabled remains the nil-able live-apply seam; it is a no-op today
// because a live screen cannot switch alternate-screen mode.
func (p *SettingsPage) toggleFullscreen() {
	if p.rendering == nil {
		p.reloadRendering()
	}

	next := &config.Rendering{Fullscreen: !p.rendering.Fullscreen}

	if p.cb.SetFullscreenEnabled != nil {
		p.cb.SetFullscreenEnabled(next.Fullscreen)
	}

	if p.cb.SaveRendering != nil {
		if err := p.cb.SaveRendering(next); err != nil {
			p.rendering = next
			p.renderWithError(fmt.Sprintf("Failed to save: %v", err))
			return
		}
	}
	p.rendering = next
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

	var sb strings.Builder

	// -- Mouse & Rendering (headline section) --
	// Two checkboxes, each carrying its own tradeoff copy. The value here is the
	// copy, not just the switch: mouse control trades native drag-to-copy for
	// GUI-feel, and fullscreen rendering trades scrollback for a flicker-free app
	// screen. A bare toggle would just relocate the confusion, so name the
	// tradeoff and the bypass/consequence inline.
	sb.WriteString("\n [yellow::b]Mouse & Rendering[-:-:-]\n")
	sb.WriteString(" [gray]─────────────────[-]\n\n")

	// Checkbox 1: Mouse control.
	mouseEnabled := p.mouse != nil && p.mouse.Enabled
	sb.WriteString(fmt.Sprintf("   %s [white::b]Mouse control[-:-:-]            Click tabs, peers, and Send instead of memorizing keys.\n", checkbox(mouseEnabled)))
	sb.WriteString("                                Tradeoff: your terminal's drag-to-copy stops working while\n")
	sb.WriteString("                                this is on. To copy anyway, hold:\n")
	sb.WriteString("                                  [white]• Shift[-]        (most terminals)\n")
	sb.WriteString("                                  [white]• Fn[-]           (macOS Terminal.app)\n")
	sb.WriteString("                                  [white]• Option[-]        (iTerm2)\n")
	sb.WriteString("   [gray]press[-] [yellow::b]m[-:-:-] [gray]to toggle (applies immediately)[-]\n\n")

	// Checkbox 2: Fullscreen rendering.
	fullscreenEnabled := p.rendering != nil && p.rendering.Fullscreen
	sb.WriteString(fmt.Sprintf("   %s [white::b]Fullscreen rendering[-:-:-]     Flicker-free, app-like. Off = output goes to normal\n", checkbox(fullscreenEnabled)))
	sb.WriteString("                                scrollback (easier to scroll + copy long history).\n")
	sb.WriteString("   [gray]press[-] [yellow::b]f[-:-:-] [gray]to toggle (saved; restart to apply)[-]\n")

	enabled := p.telemetry != nil && p.telemetry.AnonTelemetryEnabled

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

// checkbox renders a colored checkbox glyph for a boolean setting: a green
// checkmark when on, a dim empty box when off.
func checkbox(on bool) string {
	if on {
		return "[green::b][✓][-:-:-]"
	}
	return "[gray][ ][-]"
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
