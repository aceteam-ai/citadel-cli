package controlcenter

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/proxmox"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// ProxmoxPage displays a table of Proxmox VMs and containers with
// keyboard-driven actions for starting, stopping, and rebooting guests.
type ProxmoxPage struct {
	name  string
	title string
	app   *tview.Application

	// Proxmox client
	client    *proxmox.Client
	nodeName  string // Proxmox node to query
	configDir string // dir holding proxmox.json (drives this tab)

	// confirmFn shows a modal yes/no dialog. Wired from the ControlCenter so the
	// page can prompt for destructive actions (forget) without owning root/focus.
	confirmFn func(prompt, confirmLabel string, onConfirm func())

	// UI components
	root       *tview.Flex
	statusView *tview.TextView
	configView *tview.TextView
	guestTable *tview.Table
	detailView *tview.TextView
	helpBar    *tview.TextView

	// Data
	mu     sync.Mutex
	guests []proxmox.Guest
	nodes  []proxmox.Node
	active bool
	stopCh chan struct{}

	// Activity callback for logging actions to the main TUI
	activityFn func(level, msg string)
}

// ProxmoxPageConfig holds configuration for creating a ProxmoxPage.
type ProxmoxPageConfig struct {
	Client     *proxmox.Client
	NodeName   string // Proxmox node name (auto-detected if empty)
	ConfigDir  string // dir holding proxmox.json (for path display + forget)
	ActivityFn func(level, msg string)
	// ConfirmFn shows a modal yes/no dialog for destructive actions. Optional:
	// when nil, the forget action logs guidance instead of prompting.
	ConfirmFn func(prompt, confirmLabel string, onConfirm func())
}

// NewProxmoxPage creates a new Proxmox management page.
func NewProxmoxPage(cfg ProxmoxPageConfig) *ProxmoxPage {
	activityFn := cfg.ActivityFn
	if activityFn == nil {
		activityFn = func(string, string) {}
	}
	return &ProxmoxPage{
		name:       "proxmox",
		title:      "Proxmox",
		client:     cfg.Client,
		nodeName:   cfg.NodeName,
		configDir:  cfg.ConfigDir,
		activityFn: activityFn,
		confirmFn:  cfg.ConfirmFn,
	}
}

func (p *ProxmoxPage) Name() string  { return p.name }
func (p *ProxmoxPage) Title() string { return p.title }

func (p *ProxmoxPage) Build(app *tview.Application) tview.Primitive {
	p.app = app

	// Status bar (top)
	p.statusView = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)

	// Config-path line: makes it obvious which file drives this tab and where to
	// manage it. This is the "how is it detecting the VMs?" answer — a saved
	// proxmox.json pointing at a (possibly remote) host.
	p.configView = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
	p.configView.SetText(proxmoxConfigLine(p.configDir, proxmox.IsConfigured(p.configDir)))

	// Guest table (main content)
	p.guestTable = tview.NewTable().
		SetFixed(1, 0).
		SetSelectable(true, false).
		SetSelectedStyle(tcell.StyleDefault.
			Foreground(tcell.ColorBlack).
			Background(tcell.ColorWhite))
	p.guestTable.SetBorder(true).
		SetTitle(" VMs & Containers ").
		SetTitleAlign(tview.AlignLeft)

	// Detail view (right panel, shown when Enter is pressed)
	p.detailView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true)
	p.detailView.SetBorder(true).
		SetTitle(" Details ").
		SetTitleAlign(tview.AlignLeft)

	// Help bar (bottom)
	p.helpBar = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
	p.helpBar.SetText(" [yellow]1[-]=start  [yellow]2[-]=shutdown  [yellow]3[-]=force stop  [yellow]4[-]=reboot  [yellow]5[-]=console  [yellow]Enter[-]=details  [yellow]6[-]=refresh  [yellow]7[-]=forget")

	// Main layout: table on left, detail on right
	contentFlex := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(p.guestTable, 0, 2, true).
		AddItem(p.detailView, 0, 1, false)

	p.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(p.statusView, 1, 0, false).
		AddItem(p.configView, 1, 0, false).
		AddItem(contentFlex, 0, 1, true).
		AddItem(p.helpBar, 1, 0, false)

	p.updateStatus()
	p.updateGuestTable()

	return p.root
}

func (p *ProxmoxPage) OnActivate() {
	p.mu.Lock()
	p.active = true
	p.stopCh = make(chan struct{})
	p.mu.Unlock()

	go p.pollLoop()
}

func (p *ProxmoxPage) OnDeactivate() {
	p.mu.Lock()
	p.active = false
	if p.stopCh != nil {
		close(p.stopCh)
		p.stopCh = nil
	}
	p.mu.Unlock()
}

func (p *ProxmoxPage) HandleInput(event *tcell.EventKey) *tcell.EventKey {
	// Numbered actions (numbers + arrows convention, no letter shortcuts). Arrow
	// keys / Enter select a guest; these act on the selected one.
	if event.Key() == tcell.KeyRune {
		switch event.Rune() {
		case '1':
			p.actionOnSelected("start")
			return nil
		case '2':
			p.actionOnSelected("shutdown")
			return nil
		case '3':
			p.actionOnSelected("stop")
			return nil
		case '4':
			p.actionOnSelected("reboot")
			return nil
		case '5':
			p.openConsole()
			return nil
		case '6':
			go p.refreshData()
			return nil
		case '7':
			p.forgetConnection()
			return nil
		}
	}

	if event.Key() == tcell.KeyEnter {
		p.showSelectedDetail()
		return nil
	}

	return event
}

// pollLoop refreshes guest data periodically while the page is active.
func (p *ProxmoxPage) pollLoop() {
	p.refreshData()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		p.mu.Lock()
		stopCh := p.stopCh
		p.mu.Unlock()

		select {
		case <-stopCh:
			return
		case <-ticker.C:
			p.refreshData()
		}
	}
}

func (p *ProxmoxPage) refreshData() {
	if p.client == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Auto-detect node name if not set
	if p.nodeName == "" {
		nodes, err := p.client.ListNodes(ctx)
		if err != nil {
			p.app.QueueUpdateDraw(func() {
				p.statusView.SetText(fmt.Sprintf(" [red]Proxmox error: %s[-]", err))
			})
			return
		}
		p.mu.Lock()
		p.nodes = nodes
		p.mu.Unlock()

		if len(nodes) > 0 {
			// Use the first online node, or fall back to the first node
			p.nodeName = nodes[0].Node
			for _, n := range nodes {
				if n.Status == "online" {
					p.nodeName = n.Node
					break
				}
			}
		}
	}

	if p.nodeName == "" {
		p.app.QueueUpdateDraw(func() {
			p.statusView.SetText(" [red]No Proxmox nodes found[-]")
		})
		return
	}

	guests, err := p.client.ListAllGuests(ctx, p.nodeName)
	if err != nil {
		p.app.QueueUpdateDraw(func() {
			p.statusView.SetText(fmt.Sprintf(" [red]Error listing guests: %s[-]", err))
		})
		return
	}

	// Sort by VMID
	sort.Slice(guests, func(i, j int) bool {
		return guests[i].VMID < guests[j].VMID
	})

	p.mu.Lock()
	p.guests = guests
	p.mu.Unlock()

	p.app.QueueUpdateDraw(func() {
		p.updateStatus()
		p.updateGuestTable()
	})
}

func (p *ProxmoxPage) updateStatus() {
	p.mu.Lock()
	guests := p.guests
	nodes := p.nodes
	nodeName := p.nodeName
	p.mu.Unlock()

	running := 0
	stopped := 0
	for _, g := range guests {
		if g.Status == "running" {
			running++
		} else {
			stopped++
		}
	}

	nodeInfo := ""
	for _, n := range nodes {
		if n.Node == nodeName {
			cpuPct := n.CPU * 100
			memPct := float64(0)
			if n.MaxMem > 0 {
				memPct = float64(n.Mem) / float64(n.MaxMem) * 100
			}
			nodeInfo = fmt.Sprintf("  CPU: %.0f%%  Mem: %.0f%%", cpuPct, memPct)
			break
		}
	}

	total := len(guests)
	status := fmt.Sprintf(" [green::b]Proxmox[-:-:-] [white]%s[-]  |  %d guests (%d running, %d stopped)%s",
		nodeName, total, running, stopped, nodeInfo)
	p.statusView.SetText(status)
}

func (p *ProxmoxPage) updateGuestTable() {
	p.mu.Lock()
	guests := p.guests
	p.mu.Unlock()

	// Preserve selection
	selectedRow, _ := p.guestTable.GetSelection()

	p.guestTable.Clear()

	// Header row
	headers := []string{"VMID", "Name", "Type", "Status", "CPU%", "Memory", "Uptime"}
	expansions := []int{0, 1, 0, 0, 0, 0, 0}
	for i, h := range headers {
		cell := tview.NewTableCell(h).
			SetTextColor(tcell.ColorYellow).
			SetSelectable(false).
			SetExpansion(expansions[i])
		p.guestTable.SetCell(0, i, cell)
	}

	if len(guests) == 0 {
		p.guestTable.SetCell(1, 0,
			tview.NewTableCell("  No VMs or containers found").
				SetTextColor(tcell.ColorGray).
				SetSelectable(false).
				SetExpansion(1))
		return
	}

	for i, g := range guests {
		row := i + 1

		// VMID
		p.guestTable.SetCell(row, 0,
			tview.NewTableCell(fmt.Sprintf(" %d", g.VMID)).
				SetTextColor(tcell.ColorWhite))

		// Name
		name := g.Name
		if name == "" {
			name = fmt.Sprintf("VM %d", g.VMID)
		}
		p.guestTable.SetCell(row, 1,
			tview.NewTableCell(name).
				SetTextColor(tcell.ColorWhite).
				SetExpansion(1))

		// Type
		typeColor := tcell.ColorAqua
		typeLabel := "VM"
		if g.Type == "lxc" {
			typeColor = tcell.ColorYellow
			typeLabel = "CT"
		}
		p.guestTable.SetCell(row, 2,
			tview.NewTableCell(typeLabel).
				SetTextColor(typeColor))

		// Status
		statusColor := tcell.ColorGreen
		statusText := g.Status
		switch g.Status {
		case "running":
			statusColor = tcell.ColorGreen
		case "stopped":
			statusColor = tcell.ColorRed
		case "paused":
			statusColor = tcell.ColorYellow
		default:
			statusColor = tcell.ColorGray
		}
		p.guestTable.SetCell(row, 3,
			tview.NewTableCell(statusText).
				SetTextColor(statusColor))

		// CPU%
		cpuStr := "-"
		if g.Status == "running" && g.CPUs > 0 {
			cpuPct := g.CPU * 100
			cpuStr = fmt.Sprintf("%.0f%%", cpuPct)
		}
		p.guestTable.SetCell(row, 4,
			tview.NewTableCell(cpuStr).
				SetTextColor(tcell.ColorWhite))

		// Memory
		memStr := "-"
		if g.MaxMem > 0 {
			memStr = pmxFormatMem(g.Mem, g.MaxMem)
		}
		p.guestTable.SetCell(row, 5,
			tview.NewTableCell(memStr).
				SetTextColor(tcell.ColorWhite))

		// Uptime
		uptimeStr := "-"
		if g.Uptime > 0 {
			uptimeStr = pmxFormatUptime(g.Uptime)
		}
		p.guestTable.SetCell(row, 6,
			tview.NewTableCell(uptimeStr).
				SetTextColor(tcell.ColorWhite))
	}

	// Restore selection
	if selectedRow > 0 && selectedRow <= len(guests) {
		p.guestTable.Select(selectedRow, 0)
	} else if len(guests) > 0 {
		p.guestTable.Select(1, 0)
	}
}

// selectedGuest returns the currently selected guest, or nil if none.
func (p *ProxmoxPage) selectedGuest() *proxmox.Guest {
	row, _ := p.guestTable.GetSelection()
	if row < 1 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	idx := row - 1
	if idx >= len(p.guests) {
		return nil
	}
	g := p.guests[idx]
	return &g
}

// actionOnSelected executes a power action on the selected guest.
func (p *ProxmoxPage) actionOnSelected(action string) {
	guest := p.selectedGuest()
	if guest == nil {
		return
	}

	// Validate action vs current state
	switch action {
	case "start":
		if guest.Status == "running" {
			p.activityFn("warning", fmt.Sprintf("VM %d (%s) is already running", guest.VMID, guest.Name))
			return
		}
	case "shutdown", "stop", "reboot":
		if guest.Status != "running" {
			p.activityFn("warning", fmt.Sprintf("VM %d (%s) is not running", guest.VMID, guest.Name))
			return
		}
	}

	// Execute in background
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		var err error
		switch action {
		case "start":
			p.activityFn("info", fmt.Sprintf("Starting %s %d (%s)...", guest.Type, guest.VMID, guest.Name))
			err = p.client.StartGuest(ctx, p.nodeName, guest.Type, guest.VMID)
		case "shutdown":
			p.activityFn("info", fmt.Sprintf("Shutting down %s %d (%s)...", guest.Type, guest.VMID, guest.Name))
			err = p.client.ShutdownGuest(ctx, p.nodeName, guest.Type, guest.VMID)
		case "stop":
			p.activityFn("info", fmt.Sprintf("Force stopping %s %d (%s)...", guest.Type, guest.VMID, guest.Name))
			err = p.client.StopGuest(ctx, p.nodeName, guest.Type, guest.VMID)
		case "reboot":
			p.activityFn("info", fmt.Sprintf("Rebooting %s %d (%s)...", guest.Type, guest.VMID, guest.Name))
			err = p.client.RebootGuest(ctx, p.nodeName, guest.Type, guest.VMID)
		}

		if err != nil {
			p.activityFn("error", fmt.Sprintf("Failed to %s %s %d: %v", action, guest.Type, guest.VMID, err))
		} else {
			p.activityFn("success", fmt.Sprintf("%s %d (%s): %s command sent", guest.Type, guest.VMID, guest.Name, action))
		}

		// Refresh after a short delay to let the action take effect
		time.Sleep(2 * time.Second)
		p.refreshData()
	}()
}

// showSelectedDetail fetches and displays the config of the selected guest.
func (p *ProxmoxPage) showSelectedDetail() {
	guest := p.selectedGuest()
	if guest == nil {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		configData, err := p.client.GetGuestConfig(ctx, p.nodeName, guest.Type, guest.VMID)
		if err != nil {
			p.app.QueueUpdateDraw(func() {
				p.detailView.SetText(fmt.Sprintf("[red]Error: %s[-]", err))
			})
			return
		}

		// Also get current status
		status, statusErr := p.client.GetGuestStatus(ctx, p.nodeName, guest.Type, guest.VMID)

		p.app.QueueUpdateDraw(func() {
			text := fmt.Sprintf("[yellow::b]%s %d: %s[-:-:-]\n\n", guest.Type, guest.VMID, guest.Name)

			if statusErr == nil && status != nil {
				text += fmt.Sprintf("[white::b]Status:[-]  %s\n", pmxColorizeStatus(status.Status))
				if status.CPUs > 0 {
					text += fmt.Sprintf("[white::b]CPUs:[-]    %d\n", status.CPUs)
				}
				if status.CPU > 0 {
					text += fmt.Sprintf("[white::b]CPU:[-]     %.1f%%\n", status.CPU*100)
				}
				if status.MaxMem > 0 {
					text += fmt.Sprintf("[white::b]Memory:[-]  %s\n", pmxFormatMem(status.Mem, status.MaxMem))
				}
				if status.Uptime > 0 {
					text += fmt.Sprintf("[white::b]Uptime:[-]  %s\n", pmxFormatUptime(status.Uptime))
				}
				if status.PID > 0 {
					text += fmt.Sprintf("[white::b]PID:[-]     %d\n", status.PID)
				}
				text += "\n"
			}

			text += "[yellow::b]Configuration[-:-:-]\n"

			// Parse config as a map and display key-value pairs
			var configMap map[string]interface{}
			if err := json.Unmarshal(configData, &configMap); err == nil {
				keys := make([]string, 0, len(configMap))
				for k := range configMap {
					keys = append(keys, k)
				}
				sort.Strings(keys)

				for _, k := range keys {
					v := configMap[k]
					text += fmt.Sprintf("[aqua]%s:[-] %v\n", k, v)
				}
			} else {
				text += string(configData)
			}

			p.detailView.SetText(text)
			p.detailView.ScrollToBeginning()
		})
	}()
}

// openConsole opens the Proxmox noVNC console for the selected guest
// in the system's default browser.
func (p *ProxmoxPage) openConsole() {
	guest := p.selectedGuest()
	if guest == nil {
		return
	}

	if guest.Status != "running" {
		p.activityFn("warning", fmt.Sprintf("VM %d (%s) is not running — cannot open console", guest.VMID, guest.Name))
		return
	}

	consoleType := "kvm"
	if guest.Type == "lxc" {
		consoleType = "lxc"
	}

	baseURL := p.client.BaseURL()
	consoleURL := fmt.Sprintf("%s/?console=%s&novnc=1&vmid=%d&node=%s",
		baseURL, consoleType, guest.VMID, p.nodeName)

	p.activityFn("info", fmt.Sprintf("Opening console for %s %d (%s)...", guest.Type, guest.VMID, guest.Name))

	if err := platform.OpenURL(consoleURL); err != nil {
		p.activityFn("error", fmt.Sprintf("Failed to open browser: %v", err))
	}
}

// forgetConnection prompts to delete the saved Proxmox config and, on confirm,
// removes proxmox.json. The tab is built once at startup from the saved config,
// so it stays until the next restart — we tell the user that explicitly rather
// than faking a live hide.
func (p *ProxmoxPage) forgetConnection() {
	path := proxmox.ConfigPath(p.configDir)

	// The tab can also appear because this host is a detected Proxmox node with
	// no saved file. There is nothing to forget in that case — deleting a
	// nonexistent file would not hide the tab (detection still applies).
	if !proxmox.IsConfigured(p.configDir) {
		p.activityFn("info", "This host is a detected Proxmox node — there is no saved connection file to forget.")
		return
	}

	baseURL := ""
	if p.client != nil {
		baseURL = p.client.BaseURL()
	}
	target := baseURL
	if target == "" {
		target = "this Proxmox host"
	}

	doForget := func() {
		if err := proxmox.DeleteConfig(p.configDir); err != nil {
			p.activityFn("error", fmt.Sprintf("Failed to forget Proxmox connection: %v", err))
			return
		}
		p.activityFn("success", fmt.Sprintf("Forgot Proxmox connection to %s — deleted %s. Restart Citadel to hide the tab.", target, path))
	}

	if p.confirmFn == nil {
		// No modal host wired (e.g. tests): fall back to guidance instead of
		// silently deleting without confirmation.
		p.activityFn("info", fmt.Sprintf("To forget this Proxmox connection, run: citadel proxmox forget  (config: %s)", path))
		return
	}

	prompt := fmt.Sprintf(
		"Forget the saved Proxmox connection to %s?\n\nThis deletes %s.\nThe tab is hidden after restart.",
		target, path)
	p.confirmFn(prompt, "Forget", doForget)
}

// proxmoxConfigLine renders the config-path header shown on the Proxmox page.
// The tab appears either because a saved proxmox.json exists (hasSavedConfig) or
// because this host is itself a detected Proxmox node with no saved file. Only
// the saved-config case has a file to forget, so the [D]=forget affordance is
// shown only then; the detected case explains why there is nothing to remove.
func proxmoxConfigLine(configDir string, hasSavedConfig bool) string {
	if !hasSavedConfig {
		return " [gray]Config: (auto-detected local Proxmox — no saved config file to forget)[-]"
	}
	return fmt.Sprintf(" [gray]Config: %s   ([yellow]7[-][gray]=forget)[-]", proxmox.ConfigPath(configDir))
}

// pmxColorizeStatus wraps a status string with tview color tags.
func pmxColorizeStatus(s string) string {
	switch s {
	case "running":
		return "[green]running[-]"
	case "stopped":
		return "[red]stopped[-]"
	case "paused":
		return "[yellow]paused[-]"
	default:
		return "[gray]" + s + "[-]"
	}
}

// pmxFormatMem formats memory as "used/total" in human-readable units.
func pmxFormatMem(used, total int64) string {
	return fmt.Sprintf("%s/%s", pmxFormatBytes(used), pmxFormatBytes(total))
}

// pmxFormatBytes converts bytes to a human-readable string.
func pmxFormatBytes(b int64) string {
	const (
		kB = 1024
		mB = kB * 1024
		gB = mB * 1024
	)
	switch {
	case b >= gB:
		return fmt.Sprintf("%.1fG", float64(b)/float64(gB))
	case b >= mB:
		return fmt.Sprintf("%.0fM", float64(b)/float64(mB))
	case b >= kB:
		return fmt.Sprintf("%.0fK", float64(b)/float64(kB))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// pmxFormatUptime converts seconds to a human-readable duration string.
func pmxFormatUptime(seconds int64) string {
	d := seconds / 86400
	h := (seconds % 86400) / 3600
	m := (seconds % 3600) / 60

	if d > 0 {
		return fmt.Sprintf("%dd %dh", d, h)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
