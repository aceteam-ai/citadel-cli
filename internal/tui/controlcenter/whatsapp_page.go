package controlcenter

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// meshCGNAT is the Headscale mesh range (CGNAT 100.64.0.0/10). The aceteam
// backend's SSRF guard allows this range by default, so a bridge advertised on a
// mesh host is reachable with NO extra backend flag.
var _, meshCGNAT, _ = net.ParseCIDR("100.64.0.0/10")

// isNonMeshPrivateHost reports whether the host in apiURL is a private RFC1918
// address that is NOT on the mesh (100.64.0.0/10). Only such hosts need the
// backend WHATSAPP_ALLOW_PRIVATE_NETWORK flag; mesh and public hosts do not.
// A host that cannot be parsed as an IP (empty URL, hostname, placeholder) is
// treated as non-private so no flag hint is shown.
func isNonMeshPrivateHost(apiURL string) bool {
	if apiURL == "" {
		return false
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if meshCGNAT != nil && meshCGNAT.Contains(ip) {
		return false
	}
	return ip.IsPrivate()
}

// WhatsAppStatus is a snapshot of the bridge module state, produced by the
// WhatsAppCallbacks.Status hook (wired from cmd so this page never imports
// node-config/network/docker logic directly).
type WhatsAppStatus struct {
	// Deployed is true once the bridge compose file has been materialized.
	Deployed bool
	// Running is true when the bridge container is in the running state.
	Running bool
	// Reachable is true when GET / returns 200.
	Reachable bool
	// LoggedIn is true when the provisioned tenant is linked to WhatsApp.
	LoggedIn bool
	// APIURL is the mesh api_url the aceteam backend should register, or "".
	APIURL string
	// APIKey is the provisioned tenant's data-plane key, or "".
	APIKey string
	// Err is a human-readable error from the most recent probe, or "".
	Err string
}

// WhatsAppCallbacks holds the lifecycle hooks for the WhatsApp page. They are
// wired from cmd so the page stays free of platform/docker/network imports.
type WhatsAppCallbacks struct {
	// Status returns the current bridge state (fast, read-only).
	Status func() WhatsAppStatus
	// Deploy deploys+starts the bridge and provisions a tenant, returning the
	// api_url + api_key. It may block for up to ~90s; the page calls it off the
	// UI goroutine.
	Deploy func() (apiURL, apiKey string, err error)
	// Stop stops the bridge stack (auth state is preserved).
	Stop func() error
	// QRBlocks returns the pairing QR rendered as plain block characters (no
	// ANSI), suitable for a TextView, or "" if the tenant is already linked.
	QRBlocks func() (string, error)
}

// WhatsAppPage implements the Page interface for the WhatsApp bridge tab. It
// shows deploy/start/stop controls, the connect details (api_url + api_key),
// and the pairing QR so a phone can be linked without leaving the TUI.
type WhatsAppPage struct {
	app *tview.Application
	cb  WhatsAppCallbacks

	// UI
	root      *tview.Flex
	statusBox *tview.TextView
	qrBox     *tview.TextView

	// State
	mu      sync.Mutex
	status  WhatsAppStatus
	qr      string
	busy    bool   // an action (deploy/stop) is in flight
	busyMsg string // label shown while busy
	active  bool
	stopCh  chan struct{}
}

// NewWhatsAppPage creates a WhatsApp bridge page wired to the given callbacks.
func NewWhatsAppPage(cb WhatsAppCallbacks) *WhatsAppPage {
	return &WhatsAppPage{cb: cb}
}

// Name implements Page.
func (p *WhatsAppPage) Name() string { return "whatsapp" }

// Title implements Page.
func (p *WhatsAppPage) Title() string { return "WhatsApp" }

// Build implements Page.
func (p *WhatsAppPage) Build(app *tview.Application) tview.Primitive {
	p.app = app

	p.statusBox = tview.NewTextView()
	p.statusBox.SetDynamicColors(true)
	p.statusBox.SetScrollable(true)
	p.statusBox.SetBorder(true)
	p.statusBox.SetTitle(" WhatsApp Bridge ")
	p.statusBox.SetTitleAlign(tview.AlignLeft)

	p.qrBox = tview.NewTextView()
	p.qrBox.SetDynamicColors(false)
	p.qrBox.SetScrollable(true)
	p.qrBox.SetBorder(true)
	p.qrBox.SetTitle(" Pairing QR ")
	p.qrBox.SetTitleAlign(tview.AlignLeft)

	p.root = tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(p.statusBox, 0, 1, true).
		AddItem(p.qrBox, 0, 1, false)

	p.render()
	return p.root
}

// OnActivate implements Page.
func (p *WhatsAppPage) OnActivate() {
	p.mu.Lock()
	p.active = true
	p.stopCh = make(chan struct{})
	p.mu.Unlock()

	go p.refresh()
	go p.pollLoop()

	if p.app != nil && p.statusBox != nil {
		p.app.SetFocus(p.statusBox)
	}
}

// OnDeactivate implements Page.
func (p *WhatsAppPage) OnDeactivate() {
	p.mu.Lock()
	p.active = false
	if p.stopCh != nil {
		close(p.stopCh)
		p.stopCh = nil
	}
	p.mu.Unlock()
}

// HandleInput implements Page. Numbered actions: 1=deploy/start, 2=stop,
// 3=refresh QR, 4=refresh. Every action runs off the tview event-loop goroutine
// (via `go p.doX()`): each helper calls queueRender -> app.QueueUpdateDraw, and
// calling QueueUpdateDraw from the event-loop goroutine deadlocks the whole TUI
// (same class as the chat deadlock in #402). Dispatching to a goroutine keeps
// this handler render-free and safe-by-construction.
func (p *WhatsAppPage) HandleInput(event *tcell.EventKey) *tcell.EventKey {
	if event.Key() != tcell.KeyRune {
		return event
	}
	switch event.Rune() {
	case '1':
		go p.doDeploy()
		return nil
	case '2':
		go p.doStop()
		return nil
	case '3':
		go p.refreshQR()
		return nil
	case '4':
		go p.refresh()
		return nil
	}
	return event
}

// pollLoop refreshes status every 3 seconds while the page is active.
func (p *WhatsAppPage) pollLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		p.mu.Lock()
		stopCh := p.stopCh
		p.mu.Unlock()
		if stopCh == nil {
			return
		}
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			p.refresh()
		}
	}
}

// refresh probes status (and QR if not linked) and redraws.
func (p *WhatsAppPage) refresh() {
	if p.cb.Status == nil {
		return
	}
	st := p.cb.Status()

	p.mu.Lock()
	p.status = st
	busy := p.busy
	p.mu.Unlock()

	// Refresh the QR opportunistically when reachable and not yet linked.
	if !busy && st.Reachable && !st.LoggedIn {
		p.refreshQR()
	} else if st.LoggedIn {
		p.mu.Lock()
		p.qr = ""
		p.mu.Unlock()
	}
	p.queueRender()
}

// refreshQR fetches and stores the QR blocks.
func (p *WhatsAppPage) refreshQR() {
	if p.cb.QRBlocks == nil {
		return
	}
	qr, err := p.cb.QRBlocks()
	p.mu.Lock()
	if err != nil {
		p.qr = ""
	} else {
		p.qr = qr
	}
	p.mu.Unlock()
	p.queueRender()
}

// doDeploy runs the deploy action off the UI goroutine.
func (p *WhatsAppPage) doDeploy() {
	p.mu.Lock()
	if p.busy {
		p.mu.Unlock()
		return
	}
	p.busy = true
	p.busyMsg = "Deploying bridge (this can take ~30-90s on first run)..."
	p.mu.Unlock()
	p.queueRender()

	go func() {
		var err error
		if p.cb.Deploy != nil {
			_, _, err = p.cb.Deploy()
		}
		p.mu.Lock()
		p.busy = false
		p.busyMsg = ""
		if err != nil {
			p.status.Err = err.Error()
		}
		p.mu.Unlock()
		p.refresh()
	}()
}

// doStop runs the stop action off the UI goroutine.
func (p *WhatsAppPage) doStop() {
	p.mu.Lock()
	if p.busy {
		p.mu.Unlock()
		return
	}
	p.busy = true
	p.busyMsg = "Stopping bridge..."
	p.mu.Unlock()
	p.queueRender()

	go func() {
		var err error
		if p.cb.Stop != nil {
			err = p.cb.Stop()
		}
		p.mu.Lock()
		p.busy = false
		p.busyMsg = ""
		p.qr = ""
		if err != nil {
			p.status.Err = err.Error()
		}
		p.mu.Unlock()
		p.refresh()
	}()
}

// queueRender redraws on the UI goroutine if the page is live.
func (p *WhatsAppPage) queueRender() {
	if p.app == nil {
		return
	}
	p.app.QueueUpdateDraw(func() { p.render() })
}

// render redraws the status and QR panes from current state.
func (p *WhatsAppPage) render() {
	if p.statusBox == nil || p.qrBox == nil {
		return
	}
	p.mu.Lock()
	st := p.status
	qr := p.qr
	busy := p.busy
	busyMsg := p.busyMsg
	p.mu.Unlock()

	var sb strings.Builder
	sb.WriteString("\n [yellow::b]WhatsApp Bridge[-:-:-]  [gray](community module)[-]\n\n")

	// State line.
	switch {
	case !st.Deployed:
		sb.WriteString("   Status:  [gray]not deployed[-]\n")
	case st.Running && st.Reachable:
		sb.WriteString("   Status:  [green::b]● running[-:-:-]\n")
	case st.Running:
		sb.WriteString("   Status:  [yellow::b]◐ starting[-:-:-]\n")
	default:
		sb.WriteString("   Status:  [red::b]○ stopped[-:-:-]\n")
	}

	if st.Deployed && st.Reachable {
		if st.LoggedIn {
			sb.WriteString("   Phone:   [green::b]linked[-:-:-]\n")
		} else {
			sb.WriteString("   Phone:   [yellow]not linked -- scan the QR ➜[-]\n")
		}
	}

	// Connect details.
	if st.APIURL != "" || st.APIKey != "" {
		sb.WriteString("\n [white::b]Register in AceTeam[-:-:-]\n")
		if st.APIURL != "" {
			sb.WriteString(fmt.Sprintf("   api_url:  [aqua]%s[-]\n", st.APIURL))
		}
		if st.APIKey != "" {
			sb.WriteString(fmt.Sprintf("   api_key:  [aqua]%s[-]\n", st.APIKey))
		}
		sb.WriteString("   [gray]Call whatsapp_connect(api_url, api_key) in AceTeam.[-]\n")
		if isNonMeshPrivateHost(st.APIURL) {
			sb.WriteString("   [yellow]This is a non-mesh private host, so the backend[-]\n")
			sb.WriteString("   [yellow]needs WHATSAPP_ALLOW_PRIVATE_NETWORK=true to dial it.[-]\n")
		} else {
			sb.WriteString("   [gray]Reachable over the mesh by default -- no backend flag needed.[-]\n")
		}
	}

	// Busy / error.
	if busy {
		sb.WriteString(fmt.Sprintf("\n   [yellow]⏳ %s[-]\n", tview.Escape(busyMsg)))
	}
	if st.Err != "" && !busy {
		sb.WriteString(fmt.Sprintf("\n   [red]%s[-]\n", tview.Escape(st.Err)))
	}

	// Controls. Numbered actions only (numbers + arrows convention) so there is
	// nothing obscure to hunt for and every module page reads the same way.
	sb.WriteString("\n [gray]──────────────────────────────[-]\n")
	sb.WriteString("   [yellow::b][1][-:-:-] deploy/start    [yellow::b][2][-:-:-] stop\n")
	sb.WriteString("   [yellow::b][3][-:-:-] refresh QR       [yellow::b][4][-:-:-] refresh\n")
	sb.WriteString("   [gray]Community module — opened from the Modules[-]\n")
	sb.WriteString("   [gray]tab. Press Tab to return to the tab bar.[-]\n")

	p.statusBox.SetText(sb.String())

	// QR pane.
	if qr != "" {
		p.qrBox.SetText("\n  Scan with WhatsApp ▸ Linked Devices ▸ Link a Device:\n\n" + qr)
	} else if st.LoggedIn {
		p.qrBox.SetText("\n  [linked] No QR needed -- this number is connected.")
	} else if st.Deployed && st.Reachable {
		p.qrBox.SetText("\n  Generating QR... press [3] to refresh.")
	} else {
		p.qrBox.SetText("\n  Deploy the bridge (press [1]) to generate a pairing QR.")
	}
}
