// Package controlcenter provides the unified Citadel control center TUI.
package controlcenter

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// ActivityEntry represents a log entry in the activity feed
type ActivityEntry struct {
	Time    time.Time
	Level   string // "info", "success", "warning", "error"
	Message string
}

// JobStats holds job queue statistics
type JobStats struct {
	Pending    int64
	Processing int64
	Completed  int64
	Failed     int64
}

// StatusData holds all the data for the control center
type StatusData struct {
	NodeName   string
	NodeIP     string
	OrgID      string
	Connected  bool
	Version    string

	// System vitals
	CPUPercent    float64
	MemoryPercent float64
	MemoryUsed    string
	MemoryTotal   string
	DiskPercent   float64
	DiskUsed      string
	DiskTotal     string

	// GPU
	GPUName        string
	GPUUtilization float64
	GPUMemory      string
	GPUTemp        string

	// Services
	Services []ServiceInfo

	// Peers
	Peers []PeerInfo

	// Jobs
	Jobs JobStats

	// Worker status
	WorkerRunning bool
	WorkerQueue   string
}

// ServiceInfo holds service information
type ServiceInfo struct {
	Name   string
	Status string // "running", "stopped", "error"
	Uptime string
}

// PeerInfo holds peer information
type PeerInfo struct {
	Hostname string
	IP       string
	Online   bool
	Latency  string
}

// ControlCenter is the main TUI application
type ControlCenter struct {
	app  *tview.Application
	data StatusData

	// Callbacks
	refreshFn      func() (StatusData, error)
	startServiceFn func(name string) error
	stopServiceFn  func(name string) error
	viewLogsFn     func(name string) (string, error)

	// UI components
	mainFlex     *tview.Flex
	nodePanel    *tview.TextView
	vitalsPanel  *tview.TextView
	servicesView *tview.Table
	jobsPanel    *tview.TextView
	activityView *tview.TextView
	peersView    *tview.Table
	statusBar    *tview.TextView
	helpBar      *tview.TextView

	// State
	activities    []ActivityEntry
	activityMu    sync.Mutex
	autoRefresh   bool
	stopChan      chan struct{}
	selectedPanel int
	panels        []tview.Primitive
}

// Config holds control center configuration
type Config struct {
	Version        string
	RefreshFn      func() (StatusData, error)
	StartServiceFn func(name string) error
	StopServiceFn  func(name string) error
	ViewLogsFn     func(name string) (string, error)
}

// New creates a new control center
func New(cfg Config) *ControlCenter {
	return &ControlCenter{
		autoRefresh: true,
		stopChan:    make(chan struct{}),
		activities:  make([]ActivityEntry, 0, 100),
		data:        StatusData{Version: cfg.Version},
		refreshFn:   cfg.RefreshFn,
		startServiceFn: cfg.StartServiceFn,
		stopServiceFn:  cfg.StopServiceFn,
		viewLogsFn:     cfg.ViewLogsFn,
	}
}

// AddActivity adds an entry to the activity log
func (cc *ControlCenter) AddActivity(level, message string) {
	cc.activityMu.Lock()
	defer cc.activityMu.Unlock()

	entry := ActivityEntry{
		Time:    time.Now(),
		Level:   level,
		Message: message,
	}

	// Prepend (newest first)
	cc.activities = append([]ActivityEntry{entry}, cc.activities...)

	// Keep only last 100
	if len(cc.activities) > 100 {
		cc.activities = cc.activities[:100]
	}

	// Update UI if running
	if cc.app != nil && cc.activityView != nil {
		cc.app.QueueUpdateDraw(func() {
			cc.updateActivityView()
		})
	}
}

// Run starts the control center
func (cc *ControlCenter) Run() error {
	cc.app = tview.NewApplication()
	cc.buildUI()
	cc.updateAllPanels()

	// Initial activity
	cc.AddActivity("info", "Control center started")

	// Start auto-refresh
	go cc.autoRefreshLoop()

	// Key bindings
	cc.app.SetInputCapture(cc.handleInput)

	return cc.app.Run()
}

// Stop stops the control center
func (cc *ControlCenter) Stop() {
	close(cc.stopChan)
	if cc.app != nil {
		cc.app.Stop()
	}
}

func (cc *ControlCenter) buildUI() {
	// Header
	header := tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)
	header.SetText(fmt.Sprintf("\n[::b]⚡ CITADEL CONTROL CENTER[::-] [gray]%s[-]", cc.data.Version))

	// Node info (left side of top)
	cc.nodePanel = tview.NewTextView().
		SetDynamicColors(true)
	cc.nodePanel.SetBorder(true).SetTitle(" Node ")

	// Vitals (right side of top)
	cc.vitalsPanel = tview.NewTextView().
		SetDynamicColors(true)
	cc.vitalsPanel.SetBorder(true).SetTitle(" System ")

	// Top row
	topRow := tview.NewFlex().
		AddItem(cc.nodePanel, 0, 1, false).
		AddItem(cc.vitalsPanel, 0, 1, false)

	// Services table
	cc.servicesView = tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false)
	cc.servicesView.SetBorder(true).SetTitle(" Services [Enter: toggle] ")
	cc.servicesView.SetSelectedFunc(cc.onServiceSelected)

	// Jobs panel
	cc.jobsPanel = tview.NewTextView().
		SetDynamicColors(true)
	cc.jobsPanel.SetBorder(true).SetTitle(" Jobs ")

	// Middle row
	middleRow := tview.NewFlex().
		AddItem(cc.servicesView, 0, 1, true).
		AddItem(cc.jobsPanel, 30, 0, false)

	// Peers table
	cc.peersView = tview.NewTable().
		SetBorders(false).
		SetSelectable(false, false)
	cc.peersView.SetBorder(true).SetTitle(" Network Peers ")

	// Activity log
	cc.activityView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true)
	cc.activityView.SetBorder(true).SetTitle(" Activity ")

	// Bottom section (peers + activity)
	bottomRow := tview.NewFlex().
		AddItem(cc.peersView, 0, 1, false).
		AddItem(cc.activityView, 0, 1, false)

	// Help bar
	cc.helpBar = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)
	cc.updateHelpBar()

	// Status bar
	cc.statusBar = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)

	// Main layout
	cc.mainFlex = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(header, 3, 0, false).
		AddItem(topRow, 7, 0, false).
		AddItem(middleRow, 0, 1, true).
		AddItem(bottomRow, 10, 0, false).
		AddItem(cc.helpBar, 1, 0, false).
		AddItem(cc.statusBar, 1, 0, false)

	// Track focusable panels
	cc.panels = []tview.Primitive{cc.servicesView}

	cc.app.SetRoot(cc.mainFlex, true)
	cc.app.SetFocus(cc.servicesView)
}

func (cc *ControlCenter) handleInput(event *tcell.EventKey) *tcell.EventKey {
	switch event.Key() {
	case tcell.KeyEsc:
		cc.showQuitConfirm()
		return nil
	case tcell.KeyRune:
		switch event.Rune() {
		case 'q', 'Q':
			cc.showQuitConfirm()
			return nil
		case 'r', 'R':
			go cc.refresh()
			return nil
		case 'a', 'A':
			cc.autoRefresh = !cc.autoRefresh
			cc.updateStatusBar()
			return nil
		case 'l', 'L':
			cc.showLogsModal()
			return nil
		}
	}
	return event
}

func (cc *ControlCenter) showQuitConfirm() {
	modal := tview.NewModal().
		SetText("Exit Citadel Control Center?").
		AddButtons([]string{"Cancel", "Exit"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			if buttonLabel == "Exit" {
				cc.Stop()
			} else {
				cc.app.SetRoot(cc.mainFlex, true)
				cc.app.SetFocus(cc.servicesView)
			}
		})
	cc.app.SetRoot(modal, true)
}

func (cc *ControlCenter) showLogsModal() {
	if len(cc.data.Services) == 0 {
		return
	}

	// Get selected service
	row, _ := cc.servicesView.GetSelection()
	if row < 1 || row > len(cc.data.Services) {
		return
	}
	svc := cc.data.Services[row-1]

	// Show loading
	cc.AddActivity("info", fmt.Sprintf("Fetching logs for %s...", svc.Name))

	if cc.viewLogsFn == nil {
		return
	}

	logs, err := cc.viewLogsFn(svc.Name)
	if err != nil {
		cc.AddActivity("error", fmt.Sprintf("Failed to get logs: %v", err))
		return
	}

	// Create logs modal
	logView := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetText(logs)
	logView.SetBorder(true).SetTitle(fmt.Sprintf(" Logs: %s [Esc to close] ", svc.Name))

	logView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			cc.app.SetRoot(cc.mainFlex, true)
			cc.app.SetFocus(cc.servicesView)
			return nil
		}
		return event
	})

	cc.app.SetRoot(logView, true)
	cc.app.SetFocus(logView)
}

func (cc *ControlCenter) onServiceSelected(row, col int) {
	if row < 1 || row > len(cc.data.Services) {
		return
	}

	svc := cc.data.Services[row-1]

	var action string
	var actionFn func(string) error

	if svc.Status == "running" {
		action = "Stop"
		actionFn = cc.stopServiceFn
	} else {
		action = "Start"
		actionFn = cc.startServiceFn
	}

	if actionFn == nil {
		return
	}

	modal := tview.NewModal().
		SetText(fmt.Sprintf("%s service '%s'?", action, svc.Name)).
		AddButtons([]string{"Cancel", action}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			cc.app.SetRoot(cc.mainFlex, true)
			cc.app.SetFocus(cc.servicesView)

			if buttonLabel == action {
				cc.AddActivity("info", fmt.Sprintf("%sing %s...", action, svc.Name))
				go func() {
					if err := actionFn(svc.Name); err != nil {
						cc.AddActivity("error", fmt.Sprintf("Failed to %s %s: %v", strings.ToLower(action), svc.Name, err))
					} else {
						cc.AddActivity("success", fmt.Sprintf("%s %sed", svc.Name, strings.ToLower(action)))
						cc.refresh()
					}
				}()
			}
		})
	cc.app.SetRoot(modal, true)
}

func (cc *ControlCenter) updateAllPanels() {
	cc.updateNodePanel()
	cc.updateVitalsPanel()
	cc.updateServicesView()
	cc.updateJobsPanel()
	cc.updatePeersView()
	cc.updateActivityView()
	cc.updateStatusBar()
}

func (cc *ControlCenter) updateNodePanel() {
	var sb strings.Builder

	nodeName := cc.data.NodeName
	if nodeName == "" {
		nodeName = "unknown"
	}
	nodeIP := cc.data.NodeIP
	if nodeIP == "" {
		nodeIP = "-"
	}

	statusIcon := "[red]●[-]"
	statusText := "OFFLINE"
	if cc.data.Connected {
		statusIcon = "[green]●[-]"
		statusText = "ONLINE"
	}

	sb.WriteString(fmt.Sprintf(" [yellow]Node:[-]   %s\n", nodeName))
	sb.WriteString(fmt.Sprintf(" [yellow]IP:[-]     %s\n", nodeIP))
	if cc.data.OrgID != "" {
		sb.WriteString(fmt.Sprintf(" [yellow]Org:[-]    %s\n", cc.data.OrgID))
	}
	sb.WriteString(fmt.Sprintf(" [yellow]Status:[-] %s %s", statusIcon, statusText))

	cc.nodePanel.SetText(sb.String())
}

func (cc *ControlCenter) updateVitalsPanel() {
	var sb strings.Builder

	sb.WriteString(cc.formatVitalLine("CPU", cc.data.CPUPercent, ""))
	sb.WriteString(cc.formatVitalLine("Mem", cc.data.MemoryPercent, cc.data.MemoryUsed))
	sb.WriteString(cc.formatVitalLine("Disk", cc.data.DiskPercent, cc.data.DiskUsed))

	if cc.data.GPUName != "" {
		sb.WriteString(cc.formatVitalLine("GPU", cc.data.GPUUtilization, cc.data.GPUTemp))
	}

	cc.vitalsPanel.SetText(sb.String())
}

func (cc *ControlCenter) formatVitalLine(label string, percent float64, extra string) string {
	barWidth := 15
	filled := min(int(percent/100.0*float64(barWidth)), barWidth)
	empty := barWidth - filled

	color := "green"
	if percent >= 90 {
		color = "red"
	} else if percent >= 75 {
		color = "yellow"
	}

	bar := fmt.Sprintf("[%s]%s[-][gray]%s[-]", color, strings.Repeat("█", filled), strings.Repeat("░", empty))
	pct := fmt.Sprintf("[%s]%5.1f%%[-]", color, percent)

	line := fmt.Sprintf(" [yellow]%-5s[-] %s %s", label, bar, pct)
	if extra != "" {
		line += fmt.Sprintf(" [gray]%s[-]", extra)
	}
	return line + "\n"
}

func (cc *ControlCenter) updateServicesView() {
	cc.servicesView.Clear()

	// Header
	headers := []string{"SERVICE", "STATUS", "UPTIME"}
	for i, h := range headers {
		cell := tview.NewTableCell("[yellow::b]" + h + "[-:-:-]").
			SetSelectable(false).
			SetAlign(tview.AlignLeft)
		cc.servicesView.SetCell(0, i, cell)
	}

	if len(cc.data.Services) == 0 {
		cc.servicesView.SetCell(1, 0, tview.NewTableCell("[gray]No services configured[-]").SetSelectable(false))
		return
	}

	for i, svc := range cc.data.Services {
		row := i + 1

		// Name
		cc.servicesView.SetCell(row, 0, tview.NewTableCell(" "+svc.Name).SetSelectable(true))

		// Status
		var statusCell string
		switch svc.Status {
		case "running":
			statusCell = "[green]● running[-]"
		case "stopped":
			statusCell = "[gray]○ stopped[-]"
		case "error":
			statusCell = "[red]✗ error[-]"
		default:
			statusCell = "[yellow]? " + svc.Status + "[-]"
		}
		cc.servicesView.SetCell(row, 1, tview.NewTableCell(statusCell).SetSelectable(true))

		// Uptime
		uptime := svc.Uptime
		if uptime == "" {
			uptime = "-"
		}
		cc.servicesView.SetCell(row, 2, tview.NewTableCell("[gray]"+uptime+"[-]").SetSelectable(true))
	}

	cc.servicesView.Select(1, 0)
}

func (cc *ControlCenter) updateJobsPanel() {
	var sb strings.Builder

	workerStatus := "[red]○ stopped[-]"
	if cc.data.WorkerRunning {
		workerStatus = "[green]● running[-]"
	}

	sb.WriteString(fmt.Sprintf(" [yellow]Worker:[-]  %s\n", workerStatus))
	if cc.data.WorkerQueue != "" {
		sb.WriteString(fmt.Sprintf(" [yellow]Queue:[-]   [gray]%s[-]\n", cc.data.WorkerQueue))
	}
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf(" [yellow]Pending:[-]    [white]%d[-]\n", cc.data.Jobs.Pending))
	sb.WriteString(fmt.Sprintf(" [yellow]Processing:[-] [cyan]%d[-]\n", cc.data.Jobs.Processing))
	sb.WriteString(fmt.Sprintf(" [yellow]Completed:[-]  [green]%d[-]\n", cc.data.Jobs.Completed))
	if cc.data.Jobs.Failed > 0 {
		sb.WriteString(fmt.Sprintf(" [yellow]Failed:[-]     [red]%d[-]\n", cc.data.Jobs.Failed))
	}

	cc.jobsPanel.SetText(sb.String())
}

func (cc *ControlCenter) updatePeersView() {
	cc.peersView.Clear()

	headers := []string{"", "HOSTNAME", "IP", "LATENCY"}
	for i, h := range headers {
		cell := tview.NewTableCell("[yellow::b]" + h + "[-:-:-]").
			SetSelectable(false)
		cc.peersView.SetCell(0, i, cell)
	}

	if len(cc.data.Peers) == 0 {
		cc.peersView.SetCell(1, 0, tview.NewTableCell("").SetSelectable(false))
		cc.peersView.SetCell(1, 1, tview.NewTableCell("[gray]No peers connected[-]").SetSelectable(false))
		return
	}

	for i, peer := range cc.data.Peers {
		row := i + 1

		icon := "[gray]○[-]"
		if peer.Online {
			icon = "[green]●[-]"
		}

		cc.peersView.SetCell(row, 0, tview.NewTableCell(icon).SetSelectable(false))
		cc.peersView.SetCell(row, 1, tview.NewTableCell(peer.Hostname).SetSelectable(false))
		cc.peersView.SetCell(row, 2, tview.NewTableCell("[gray]"+peer.IP+"[-]").SetSelectable(false))

		latency := peer.Latency
		if latency == "" {
			latency = "-"
		}
		cc.peersView.SetCell(row, 3, tview.NewTableCell("[gray]"+latency+"[-]").SetSelectable(false))
	}
}

func (cc *ControlCenter) updateActivityView() {
	cc.activityMu.Lock()
	defer cc.activityMu.Unlock()

	var sb strings.Builder

	for _, entry := range cc.activities {
		timeStr := entry.Time.Format("15:04:05")

		color := "white"
		icon := "•"
		switch entry.Level {
		case "success":
			color = "green"
			icon = "✓"
		case "warning":
			color = "yellow"
			icon = "⚠"
		case "error":
			color = "red"
			icon = "✗"
		case "info":
			color = "gray"
			icon = "•"
		}

		sb.WriteString(fmt.Sprintf(" [gray]%s[-] [%s]%s[-] %s\n", timeStr, color, icon, entry.Message))
	}

	cc.activityView.SetText(sb.String())
}

func (cc *ControlCenter) updateHelpBar() {
	cc.helpBar.SetText("[yellow::b]r[-:-:-] refresh  [yellow::b]a[-:-:-] auto  [yellow::b]l[-:-:-] logs  [yellow::b]Enter[-:-:-] start/stop  [yellow::b]q[-:-:-] quit")
}

func (cc *ControlCenter) updateStatusBar() {
	autoStr := "[red]off[-]"
	if cc.autoRefresh {
		autoStr = "[green]on[-]"
	}
	cc.statusBar.SetText(fmt.Sprintf("Auto-refresh: %s  │  Press [yellow::b]?[-:-:-] for help", autoStr))
}

func (cc *ControlCenter) refresh() {
	if cc.refreshFn == nil {
		return
	}

	data, err := cc.refreshFn()
	if err != nil {
		cc.AddActivity("error", fmt.Sprintf("Refresh failed: %v", err))
		return
	}

	cc.data = data

	cc.app.QueueUpdateDraw(func() {
		cc.updateAllPanels()
	})
}

func (cc *ControlCenter) autoRefreshLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-cc.stopChan:
			return
		case <-ticker.C:
			if cc.autoRefresh {
				cc.refresh()
			}
		}
	}
}

// UpdateData updates the control center data (thread-safe)
func (cc *ControlCenter) UpdateData(data StatusData) {
	cc.data = data
	if cc.app != nil {
		cc.app.QueueUpdateDraw(func() {
			cc.updateAllPanels()
		})
	}
}

// UpdateJobStats updates just the job statistics
func (cc *ControlCenter) UpdateJobStats(stats JobStats) {
	cc.data.Jobs = stats
	if cc.app != nil && cc.jobsPanel != nil {
		cc.app.QueueUpdateDraw(func() {
			cc.updateJobsPanel()
		})
	}
}

// SetWorkerRunning updates the worker status
func (cc *ControlCenter) SetWorkerRunning(running bool, queue string) {
	cc.data.WorkerRunning = running
	cc.data.WorkerQueue = queue
	if cc.app != nil && cc.jobsPanel != nil {
		cc.app.QueueUpdateDraw(func() {
			cc.updateJobsPanel()
		})
	}
}
