// Package controlcenter provides the unified Citadel control center TUI.
package controlcenter

import (
	"fmt"
	"runtime"
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

// RefreshInterval is the default auto-refresh interval (matches heartbeat)
const RefreshInterval = 30 * time.Second

// ControlCenter is the main TUI application
type ControlCenter struct {
	app  *tview.Application
	data StatusData

	// Callbacks
	refreshFn      func() (StatusData, error)
	startServiceFn func(name string) error
	stopServiceFn  func(name string) error

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
	cmdInput     *tview.InputField

	// State
	activities    []ActivityEntry
	activityMu    sync.Mutex
	stopChan      chan struct{}
	selectedPanel int
	panels        []tview.Primitive
	running       bool
	lastRefresh   time.Time
	inModal       bool // Track if we're in a modal (help, logs, etc.)
}

// Config holds control center configuration
type Config struct {
	Version        string
	RefreshFn      func() (StatusData, error)
	StartServiceFn func(name string) error
	StopServiceFn  func(name string) error
}

// New creates a new control center
func New(cfg Config) *ControlCenter {
	return &ControlCenter{
		stopChan:       make(chan struct{}),
		activities:     make([]ActivityEntry, 0, 100),
		data:           StatusData{Version: cfg.Version},
		refreshFn:      cfg.RefreshFn,
		startServiceFn: cfg.StartServiceFn,
		stopServiceFn:  cfg.StopServiceFn,
	}
}

// AddActivity adds an entry to the activity log
func (cc *ControlCenter) AddActivity(level, message string) {
	cc.activityMu.Lock()
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
	cc.activityMu.Unlock()

	// Update UI if running - MUST be outside the lock to avoid deadlock
	// since updateActivityView also acquires the lock
	if cc.app != nil && cc.activityView != nil && cc.running {
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

	// Key bindings
	cc.app.SetInputCapture(cc.handleInput)

	// Start background tasks after a brief delay to ensure event loop is running
	go func() {
		time.Sleep(50 * time.Millisecond)
		cc.running = true
		cc.AddActivity("info", "Control center started")
		go cc.autoRefreshLoop()
	}()

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

	// Services table (display only)
	cc.servicesView = tview.NewTable().
		SetBorders(false).
		SetSelectable(false, false)
	cc.servicesView.SetBorder(true).SetTitle(" Services ")

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

	// Command input
	cc.cmdInput = tview.NewInputField().
		SetLabel(" > ").
		SetFieldWidth(0).
		SetPlaceholder("Type command or ? for help").
		SetFieldBackgroundColor(tcell.ColorDefault).
		SetDoneFunc(cc.onCommandEntered)

	// Input container with border
	inputContainer := tview.NewFlex().
		AddItem(cc.cmdInput, 0, 1, true)
	inputContainer.SetBorder(true).SetTitle(" Command ")

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
		AddItem(inputContainer, 3, 0, false).
		AddItem(cc.helpBar, 1, 0, false).
		AddItem(cc.statusBar, 1, 0, false)

	// Track focusable panels - command input is the main interaction point
	cc.panels = []tview.Primitive{cc.cmdInput}

	cc.app.SetRoot(cc.mainFlex, true)
	cc.app.SetFocus(cc.cmdInput)
}

func (cc *ControlCenter) handleInput(event *tcell.EventKey) *tcell.EventKey {
	// If in a modal, let the modal handle all input
	if cc.inModal {
		return event
	}

	// Trap all quit attempts and show confirmation
	switch event.Key() {
	case tcell.KeyCtrlC, tcell.KeyEsc:
		cc.showQuitConfirm()
		return nil
	case tcell.KeyRune:
		switch event.Rune() {
		case '?':
			cc.showHelpModal()
			return nil
		}
	}

	// Let the input field handle everything else
	return event
}

func (cc *ControlCenter) onCommandEntered(key tcell.Key) {
	if key != tcell.KeyEnter {
		return
	}

	cmd := strings.TrimSpace(cc.cmdInput.GetText())
	cc.cmdInput.SetText("")

	if cmd == "" {
		return
	}

	// Process command
	cc.processCommand(cmd)
}

func (cc *ControlCenter) processCommand(cmd string) {
	// Remove leading / or : if present
	cmd = strings.TrimPrefix(cmd, "/")
	cmd = strings.TrimPrefix(cmd, ":")

	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return
	}

	command := strings.ToLower(parts[0])

	switch command {
	case "help", "?", "h":
		cc.showHelpModal()
	case "refresh", "r":
		go cc.refresh()
		cc.AddActivity("info", "Manual refresh triggered")
	case "quit", "q", "exit":
		cc.showQuitConfirm()
	case "start":
		if len(parts) > 1 && cc.startServiceFn != nil {
			svcName := parts[1]
			cc.AddActivity("info", fmt.Sprintf("Starting %s...", svcName))
			go func() {
				if err := cc.startServiceFn(svcName); err != nil {
					cc.AddActivity("error", fmt.Sprintf("Failed to start %s: %v", svcName, err))
				} else {
					cc.AddActivity("success", fmt.Sprintf("%s started", svcName))
					cc.refresh()
				}
			}()
		} else {
			cc.AddActivity("warning", "Usage: start <service-name>")
		}
	case "stop":
		if len(parts) > 1 && cc.stopServiceFn != nil {
			svcName := parts[1]
			cc.AddActivity("info", fmt.Sprintf("Stopping %s...", svcName))
			go func() {
				if err := cc.stopServiceFn(svcName); err != nil {
					cc.AddActivity("error", fmt.Sprintf("Failed to stop %s: %v", svcName, err))
				} else {
					cc.AddActivity("success", fmt.Sprintf("%s stopped", svcName))
					cc.refresh()
				}
			}()
		} else {
			cc.AddActivity("warning", "Usage: stop <service-name>")
		}
	case "status":
		go cc.refresh()
		cc.AddActivity("info", "Refreshing status...")
	case "login":
		cc.AddActivity("info", "Run 'citadel login' in terminal to connect")
	case "work", "worker":
		cc.AddActivity("info", "Run 'citadel work' in terminal to start worker")
	default:
		cc.AddActivity("warning", fmt.Sprintf("Unknown command: %s (type ? for help)", command))
	}
}

func (cc *ControlCenter) showQuitConfirm() {
	cc.inModal = true

	warningText := `⚠️  Are you sure you want to exit?

If you quit:
• Your services will no longer be accessible on the network
• Other nodes won't be able to connect to this machine
• Jobs won't be processed on this node

To keep Citadel running in the background, install it as a system service.`

	modal := tview.NewModal().
		SetText(warningText).
		AddButtons([]string{"Cancel", "Install Service", "Exit Anyway"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			cc.inModal = false
			switch buttonLabel {
			case "Exit Anyway":
				cc.Stop()
			case "Install Service":
				cc.app.SetRoot(cc.mainFlex, true)
				cc.app.SetFocus(cc.cmdInput)
				cc.showInstallServiceHelp()
			default:
				cc.app.SetRoot(cc.mainFlex, true)
				cc.app.SetFocus(cc.cmdInput)
			}
		})

	cc.app.SetRoot(modal, true)
	cc.app.SetFocus(modal)
}

func (cc *ControlCenter) showInstallServiceHelp() {
	cc.AddActivity("info", "To install Citadel as a system service:")
	cc.AddActivity("info", "")

	// Detect OS and show appropriate instructions
	switch {
	case isLinux():
		cc.AddActivity("info", "[Linux] Run: sudo citadel service install")
		cc.AddActivity("info", "         Or manually create a systemd unit file")
	case isDarwin():
		cc.AddActivity("info", "[macOS] Run: citadel service install")
		cc.AddActivity("info", "         Or use: brew services start citadel")
	case isWindows():
		cc.AddActivity("info", "[Windows] Run as Admin: citadel service install")
		cc.AddActivity("info", "          Creates a Windows Service")
	default:
		cc.AddActivity("info", "Run: citadel service install")
	}

	cc.AddActivity("info", "")
	cc.AddActivity("info", "This will keep Citadel running in the background.")
}

func isLinux() bool {
	return strings.Contains(strings.ToLower(runtime.GOOS), "linux")
}

func isDarwin() bool {
	return strings.Contains(strings.ToLower(runtime.GOOS), "darwin")
}

func isWindows() bool {
	return strings.Contains(strings.ToLower(runtime.GOOS), "windows")
}

func (cc *ControlCenter) showHelpModal() {
	helpText := `[yellow::b]Citadel Control Center Help[-:-:-]

[yellow]Commands:[-]
  [white]help[-]            Show this help
  [white]refresh[-]         Refresh status
  [white]start <svc>[-]     Start a service
  [white]stop <svc>[-]      Stop a service
  [white]quit[-]            Exit (with confirmation)

[yellow]Shortcuts:[-]
  [white::b]?[-:-:-]         Show this help
  [white::b]Esc/Ctrl+C[-:-:-] Quit (with confirmation)

[yellow]Background Service:[-]
  To run Citadel in the background without this UI:
  [gray]citadel service install[-]   Install as system service
  [gray]citadel --daemon[-]          Run in daemon mode (no UI)

[yellow]Other Citadel Commands:[-]
  [gray]citadel login[-]      Connect to AceTeam Network
  [gray]citadel logout[-]     Disconnect from network
  [gray]citadel work[-]       Start job worker for GPU tasks

[gray]Press Esc/q/? to close[-]`

	cc.inModal = true

	helpView := tview.NewTextView().
		SetDynamicColors(true).
		SetText(helpText)
	helpView.SetBorder(true).SetTitle(" Help ")

	helpView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc || event.Rune() == '?' || event.Rune() == 'q' {
			cc.inModal = false
			cc.app.SetRoot(cc.mainFlex, true)
			cc.app.SetFocus(cc.cmdInput)
			return nil
		}
		return event
	})

	cc.app.SetRoot(helpView, true)
	cc.app.SetFocus(helpView)
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

	// Show network connection status first
	networkStatus := "[red]● Disconnected[-]"
	networkDetail := "Run [yellow]citadel login[-] to connect"
	if cc.data.Connected {
		networkStatus = "[green]● Connected to AceTeam Network[-]"
		networkDetail = ""
		if cc.data.NodeIP != "" {
			networkDetail = fmt.Sprintf("[gray]IP: %s[-]", cc.data.NodeIP)
		}
	}

	cc.peersView.SetCell(0, 0, tview.NewTableCell(" "+networkStatus).SetSelectable(false))
	cc.peersView.SetCell(0, 1, tview.NewTableCell(networkDetail).SetSelectable(false))

	if len(cc.data.Peers) == 0 {
		if cc.data.Connected {
			cc.peersView.SetCell(1, 0, tview.NewTableCell(" [gray]No other peers on network[-]").SetSelectable(false))
		}
		return
	}

	// Headers
	headers := []string{"", "HOSTNAME", "IP", "LATENCY"}
	for i, h := range headers {
		cell := tview.NewTableCell("[yellow::b]" + h + "[-:-:-]").
			SetSelectable(false)
		cc.peersView.SetCell(1, i, cell)
	}

	for i, peer := range cc.data.Peers {
		row := i + 2 // Start after status and header

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
	cc.helpBar.SetText("[yellow::b]?[-:-:-] help  [yellow::b]Esc[-:-:-] quit  │  Type commands: [gray]start <svc>, stop <svc>, refresh, help[-]")
}

func (cc *ControlCenter) updateStatusBar() {
	lastRefreshStr := "[gray]starting...[-]"
	if !cc.lastRefresh.IsZero() {
		lastRefreshStr = "[gray]" + cc.lastRefresh.Format("15:04:05") + "[-]"
	}

	cc.statusBar.SetText(fmt.Sprintf("Refresh: [green]auto (30s)[-]  │  Last: %s  │  Press [yellow::b]?[-:-:-] for help", lastRefreshStr))
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
	cc.lastRefresh = time.Now()

	cc.app.QueueUpdateDraw(func() {
		cc.updateAllPanels()
	})
}

func (cc *ControlCenter) autoRefreshLoop() {
	ticker := time.NewTicker(RefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-cc.stopChan:
			return
		case <-ticker.C:
			cc.refresh()
		}
	}
}

// UpdateData updates the control center data (thread-safe)
func (cc *ControlCenter) UpdateData(data StatusData) {
	cc.data = data
	cc.lastRefresh = time.Now()
	if cc.app != nil && cc.running {
		cc.app.QueueUpdateDraw(func() {
			cc.updateAllPanels()
		})
	}
}

// UpdateJobStats updates just the job statistics
func (cc *ControlCenter) UpdateJobStats(stats JobStats) {
	cc.data.Jobs = stats
	if cc.app != nil && cc.jobsPanel != nil && cc.running {
		cc.app.QueueUpdateDraw(func() {
			cc.updateJobsPanel()
		})
	}
}

// SetWorkerRunning updates the worker status
func (cc *ControlCenter) SetWorkerRunning(running bool, queue string) {
	cc.data.WorkerRunning = running
	cc.data.WorkerQueue = queue
	if cc.app != nil && cc.jobsPanel != nil && cc.running {
		cc.app.QueueUpdateDraw(func() {
			cc.updateJobsPanel()
		})
	}
}
