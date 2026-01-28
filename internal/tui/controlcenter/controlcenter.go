// Package controlcenter provides the unified Citadel control center TUI.
package controlcenter

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
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

// QueueInfo holds information about a subscribed queue
type QueueInfo struct {
	Name         string
	Type         string // "redis", "api"
	Connected    bool
	PendingCount int64
}

// JobRecord tracks a processed job for history
type JobRecord struct {
	ID          string
	Type        string
	Status      string    // "success", "failed", "processing"
	StartedAt   time.Time
	CompletedAt time.Time
	Duration    time.Duration
	Error       string
}

// StatusData holds all the data for the control center
type StatusData struct {
	NodeName   string
	NodeIP     string
	OrgID      string
	Connected  bool
	Version    string

	// User info (from device auth)
	UserEmail string
	UserName  string

	// Dual connection detection (system tailscale + citadel tsnet)
	SystemTailscaleRunning bool
	SystemTailscaleIP      string
	SystemTailscaleName    string
	DualConnection         bool // Both system tailscale and citadel on same network

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

	// Demo server
	DemoServerURL string

	// Terminal server
	TerminalServerURL string

	// Worker status
	WorkerRunning bool
	WorkerQueue   string
	Queues        []QueueInfo  // All subscribed queues
	RecentJobs    []JobRecord  // Last N jobs processed

	// Heartbeat status
	HeartbeatActive bool
	LastHeartbeat   time.Time
}

// ServiceInfo holds service information
type ServiceInfo struct {
	Name   string
	Status string // "running", "stopped", "error"
	Uptime string
}

// ServiceDetailInfo holds detailed service information for the modal
type ServiceDetailInfo struct {
	ContainerID string
	Image       string
	ComposePath string
	Ports       []string
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

// PortForward represents an active port forward
type PortForward struct {
	LocalPort   int
	Description string
	Listener    interface{} // net.Listener - using interface to avoid import cycle
	StartedAt   time.Time
}

// ControlCenter is the main TUI application
type ControlCenter struct {
	app  *tview.Application
	data StatusData

	// Callbacks
	refreshFn          func() (StatusData, error)
	startServiceFn     func(name string) error
	stopServiceFn      func(name string) error
	restartServiceFn   func(name string) error     // Restart a service
	addServiceFn       func(name string) error     // Add a new service to manifest
	getServicesFn      func() []string             // Get available services
	getConfiguredFn    func() []string             // Get already configured services
	getServiceDetailFn func(name string) *ServiceDetailInfo // Get detailed service info
	getServiceLogsFn   func(name string) ([]string, error)  // Get recent service logs
	deviceAuth         DeviceAuthCallbacks         // Device authorization callbacks
	worker             WorkerCallbacks             // Worker management callbacks
	authServiceURL     string                      // URL for device auth service
	nexusURL           string                      // URL for headscale/nexus coordination server

	// UI components
	mainFlex     *tview.Flex
	nodePanel    *tview.TextView
	vitalsPanel  *tview.TextView
	servicesView *tview.Table
	jobsPanel    *tview.TextView
	actionsView  *tview.Table
	activityView *tview.TextView
	peersView    *tview.Table
	statusBar    *tview.TextView
	helpBar      *tview.TextView

	// State
	activities     []ActivityEntry
	activityMu     sync.Mutex
	stopChan       chan struct{}
	running        bool
	lastRefresh    time.Time
	inModal        bool          // Track if we're in a modal (help, quit, etc.)
	activeForwards []PortForward // Active port forwards
	focusedPane    int           // 0=services, 1=peers

	// Suggestions bar
	suggestionBar     *tview.TextView
	showingSuggestion bool
	suggestionTimer   *time.Timer

	// Job tracking
	recentJobs   []JobRecord
	recentJobsMu sync.Mutex
}

// Pane focus constants
const (
	paneNode     = 0
	paneSystem   = 1
	paneJobs     = 2
	paneServices = 3
	paneActions  = 4
	panePeers    = 5
	paneActivity = 6
	paneCount    = 7
)

// DeviceAuthConfig holds device authorization flow parameters
type DeviceAuthConfig struct {
	UserCode        string
	VerificationURI string
	DeviceCode      string
	ExpiresIn       int
	Interval        int
}

// DeviceAuthCallbacks holds callbacks for device authorization flow
type DeviceAuthCallbacks struct {
	StartFlow   func() (*DeviceAuthConfig, error)                       // Start device auth flow, returns codes
	PollToken   func(deviceCode string, interval int) (authkey string, err error) // Poll for token
	Connect     func(authkey string) error                              // Connect with authkey
	Disconnect  func() error                                            // Disconnect from network
}

// WorkerCallbacks holds callbacks for worker management
type WorkerCallbacks struct {
	Start       func(activityFn func(level, msg string)) error // Start worker with activity callback
	Stop        func() error                                    // Stop worker
	IsRunning   func() bool                                     // Check if worker is running
}

// Config holds control center configuration
type Config struct {
	Version            string
	AuthServiceURL     string                  // URL for device auth service
	NexusURL           string                  // URL for headscale/nexus coordination server
	RefreshFn          func() (StatusData, error)
	StartServiceFn     func(name string) error
	StopServiceFn      func(name string) error
	RestartServiceFn   func(name string) error     // Restart a service
	AddServiceFn       func(name string) error     // Add a new service to manifest
	GetServicesFn      func() []string             // Get available services
	GetConfiguredFn    func() []string             // Get already configured services
	GetServiceDetailFn func(name string) *ServiceDetailInfo // Get detailed service info
	GetServiceLogsFn   func(name string) ([]string, error)  // Get recent service logs
	DeviceAuth         DeviceAuthCallbacks         // Device authorization callbacks
	Worker             WorkerCallbacks             // Worker management callbacks
}

// New creates a new control center
func New(cfg Config) *ControlCenter {
	return &ControlCenter{
		stopChan:           make(chan struct{}),
		activities:         make([]ActivityEntry, 0, 100),
		activeForwards:     make([]PortForward, 0),
		data:               StatusData{Version: cfg.Version},
		refreshFn:          cfg.RefreshFn,
		startServiceFn:     cfg.StartServiceFn,
		stopServiceFn:      cfg.StopServiceFn,
		restartServiceFn:   cfg.RestartServiceFn,
		addServiceFn:       cfg.AddServiceFn,
		getServicesFn:      cfg.GetServicesFn,
		getConfiguredFn:    cfg.GetConfiguredFn,
		getServiceDetailFn: cfg.GetServiceDetailFn,
		getServiceLogsFn:   cfg.GetServiceLogsFn,
		deviceAuth:      cfg.DeviceAuth,
		worker:          cfg.Worker,
		authServiceURL:  cfg.AuthServiceURL,
		nexusURL:        cfg.NexusURL,
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

	// Update UI if running
	// Use goroutine to avoid blocking when called from input handlers
	// (QueueUpdateDraw can block if called from within the event loop)
	if cc.app != nil && cc.activityView != nil && cc.running {
		go func() {
			cc.app.QueueUpdateDraw(func() {
				cc.updateActivityView()
			})
		}()
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

		// Show context-aware suggestion after startup
		time.Sleep(500 * time.Millisecond)
		cc.app.QueueUpdateDraw(func() {
			cc.showSuggestion()
		})
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

	// Node info panel
	cc.nodePanel = tview.NewTextView().
		SetDynamicColors(true)
	cc.nodePanel.SetBorder(true).SetTitle(" Node ")

	// Vitals panel
	cc.vitalsPanel = tview.NewTextView().
		SetDynamicColors(true)
	cc.vitalsPanel.SetBorder(true).SetTitle(" System ")

	// Jobs panel
	cc.jobsPanel = tview.NewTextView().
		SetDynamicColors(true)
	cc.jobsPanel.SetBorder(true).SetTitle(" Jobs ")

	// Top row: Node + Vitals + Jobs (3 equal columns)
	topRow := tview.NewFlex().
		AddItem(cc.nodePanel, 0, 1, false).
		AddItem(cc.vitalsPanel, 0, 1, false).
		AddItem(cc.jobsPanel, 0, 1, false)

	// Services table - navigable with arrow keys
	cc.servicesView = tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false).
		SetFixed(1, 0)
	cc.servicesView.SetBorder(true).SetTitle(" Services ")
	cc.servicesView.SetSelectedStyle(tcell.StyleDefault.
		Background(tcell.ColorDarkBlue).
		Foreground(tcell.ColorWhite))

	// Actions table - selectable list of actions
	cc.actionsView = tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false)
	cc.actionsView.SetBorder(true).SetTitle(" Actions ")
	cc.actionsView.SetSelectedStyle(tcell.StyleDefault.
		Background(tcell.ColorDarkBlue).
		Foreground(tcell.ColorWhite))
	cc.updateActionsPanel()

	// Middle row: Services + Actions (2 columns)
	middleRow := tview.NewFlex().
		AddItem(cc.servicesView, 0, 1, true).
		AddItem(cc.actionsView, 0, 1, false)

	// Peers table - selectable and scrollable
	cc.peersView = tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false).
		SetFixed(1, 0)
	cc.peersView.SetBorder(true).SetTitle(" Network Peers ")
	cc.peersView.SetSelectedStyle(tcell.StyleDefault.
		Background(tcell.ColorDarkBlue).
		Foreground(tcell.ColorWhite))

	// Activity log - scrollable
	cc.activityView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true)
	cc.activityView.SetBorder(true).SetTitle(" Activity ")

	// Bottom row: Peers + Activity (2 columns)
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

	// Suggestions bar (shows context-aware tips, auto-dismisses)
	cc.suggestionBar = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)

	// Main layout - more uniform heights
	cc.mainFlex = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(header, 3, 0, false).
		AddItem(cc.suggestionBar, 1, 0, false).
		AddItem(topRow, 8, 0, false).
		AddItem(middleRow, 0, 1, true).
		AddItem(bottomRow, 0, 1, false).
		AddItem(cc.helpBar, 1, 0, false).
		AddItem(cc.statusBar, 1, 0, false)

	cc.app.SetRoot(cc.mainFlex, true)
	cc.focusedPane = paneServices
	cc.updatePaneFocus()
}

func (cc *ControlCenter) handleInput(event *tcell.EventKey) *tcell.EventKey {
	// Dismiss suggestion on any keypress
	if cc.showingSuggestion {
		cc.dismissSuggestion()
	}

	// If in a modal, let the modal handle all input
	if cc.inModal {
		return event
	}

	switch event.Key() {
	case tcell.KeyCtrlC:
		cc.Stop() // Immediate exit on Ctrl+C
		return nil
	case tcell.KeyEsc:
		cc.showQuitConfirm()
		return nil
	case tcell.KeyTab:
		// Switch to next pane
		cc.focusNextPane()
		return nil
	case tcell.KeyBacktab:
		// Switch to previous pane
		cc.focusPrevPane()
		return nil
	case tcell.KeyEnter:
		// Action depends on focused pane
		cc.handleEnter()
		return nil
	case tcell.KeyUp:
		cc.handleArrowUp()
		return nil
	case tcell.KeyDown:
		cc.handleArrowDown()
		return nil
	case tcell.KeyRune:
		switch event.Rune() {
		case 'q', 'Q':
			cc.showQuitConfirm()
			return nil
		case '?', 'h', 'H':
			cc.showHelpModal()
			return nil
		case 'r', 'R':
			go func() {
				cc.AddActivity("info", "Manual refresh triggered")
				cc.refresh()
			}()
			return nil
		case 's', 'S':
			// Start selected service
			if cc.focusedPane == paneServices {
				cc.startSelectedService()
			}
			return nil
		case 'x', 'X':
			// Stop selected service
			if cc.focusedPane == paneServices {
				cc.stopSelectedService()
			}
			return nil
		case 'j':
			// Vim-style down
			cc.handleArrowDown()
			return nil
		case 'k':
			// Vim-style up
			cc.handleArrowUp()
			return nil
		// Action menu shortcuts (0-7)
		case '1':
			cc.showAddServiceModal()
			return nil
		case '2':
			cc.showExposePortModal()
			return nil
		case '3':
			cc.showPortForwardsModal()
			return nil
		case '4':
			cc.showSSHAccessModal()
			return nil
		case '5':
			cc.pingPeers()
			return nil
		case '6':
			cc.showInstallServiceModal()
			return nil
		case '0':
			cc.showNetworkModal()
			return nil
		case 'p', 'P':
			// Ping selected peer
			cc.pingSelectedPeer()
			return nil
		case 'c':
			// Copy focused panel to file
			cc.copyFocusedPanel()
			return nil
		case 'C':
			// Copy all panels to file
			cc.copyAllPanels()
			return nil
		case 'l', 'L':
			// Copy activity logs
			cc.copyActivityLogs()
			return nil
		}
	}

	return event
}

// focusNextPane switches focus to the next pane
func (cc *ControlCenter) focusNextPane() {
	cc.focusedPane = (cc.focusedPane + 1) % paneCount
	cc.updatePaneFocus()
}

// focusPrevPane switches focus to the previous pane
func (cc *ControlCenter) focusPrevPane() {
	cc.focusedPane = (cc.focusedPane - 1 + paneCount) % paneCount
	cc.updatePaneFocus()
}

// updatePaneFocus updates the visual focus and app focus
func (cc *ControlCenter) updatePaneFocus() {
	// Reset all borders to default
	cc.nodePanel.SetBorderColor(tcell.ColorWhite)
	cc.nodePanel.SetTitle(" Node ")
	cc.vitalsPanel.SetBorderColor(tcell.ColorWhite)
	cc.vitalsPanel.SetTitle(" System ")
	cc.jobsPanel.SetBorderColor(tcell.ColorWhite)
	cc.jobsPanel.SetTitle(" Jobs ")
	cc.servicesView.SetBorderColor(tcell.ColorWhite)
	cc.servicesView.SetTitle(" Services ")
	cc.actionsView.SetBorderColor(tcell.ColorWhite)
	cc.actionsView.SetTitle(" Actions ")
	cc.peersView.SetBorderColor(tcell.ColorWhite)
	cc.peersView.SetTitle(" Peers ")
	cc.activityView.SetBorderColor(tcell.ColorWhite)
	cc.activityView.SetTitle(" Activity ")

	// Highlight focused pane
	switch cc.focusedPane {
	case paneNode:
		cc.nodePanel.SetBorderColor(tcell.ColorYellow)
		cc.nodePanel.SetTitle(" [yellow::b]Node[-:-:-] ")
		cc.app.SetFocus(cc.nodePanel)
	case paneSystem:
		cc.vitalsPanel.SetBorderColor(tcell.ColorYellow)
		cc.vitalsPanel.SetTitle(" [yellow::b]System[-:-:-] ")
		cc.app.SetFocus(cc.vitalsPanel)
	case paneJobs:
		cc.jobsPanel.SetBorderColor(tcell.ColorYellow)
		cc.jobsPanel.SetTitle(" [yellow::b]Jobs[-:-:-] ")
		cc.app.SetFocus(cc.jobsPanel)
	case paneServices:
		cc.servicesView.SetBorderColor(tcell.ColorYellow)
		cc.servicesView.SetTitle(" [yellow::b]Services[-:-:-] ")
		cc.app.SetFocus(cc.servicesView)
	case paneActions:
		cc.actionsView.SetBorderColor(tcell.ColorYellow)
		cc.actionsView.SetTitle(" [yellow::b]Actions[-:-:-] ")
		cc.app.SetFocus(cc.actionsView)
	case panePeers:
		cc.peersView.SetBorderColor(tcell.ColorYellow)
		cc.peersView.SetTitle(" [yellow::b]Peers[-:-:-] ")
		cc.app.SetFocus(cc.peersView)
	case paneActivity:
		cc.activityView.SetBorderColor(tcell.ColorYellow)
		cc.activityView.SetTitle(" [yellow::b]Activity[-:-:-] ")
		cc.app.SetFocus(cc.activityView)
	}
	cc.updateHelpBar()
}

// handleEnter handles Enter key based on focused pane
func (cc *ControlCenter) handleEnter() {
	switch cc.focusedPane {
	case paneNode:
		cc.showNodeDetailModal()
	case paneSystem:
		cc.showSystemDetailModal()
	case paneJobs:
		cc.showJobsDetailModal()
	case paneServices:
		cc.showServiceDetailModal()
	case paneActions:
		cc.executeSelectedAction()
	case panePeers:
		cc.showPeerDetailModal()
	case paneActivity:
		cc.showActivityFullScreen()
	}
}

// executeSelectedAction runs the action selected in the actions table
func (cc *ControlCenter) executeSelectedAction() {
	row, _ := cc.actionsView.GetSelection()
	actions := cc.getActions()
	if row >= 0 && row < len(actions) {
		actions[row].fn()
	}
}

// handleArrowUp handles up arrow based on focused pane
func (cc *ControlCenter) handleArrowUp() {
	switch cc.focusedPane {
	case paneServices:
		row, _ := cc.servicesView.GetSelection()
		if row > 1 {
			cc.servicesView.Select(row-1, 0)
		}
	case paneActions:
		row, _ := cc.actionsView.GetSelection()
		if row > 0 {
			cc.actionsView.Select(row-1, 0)
		}
	case panePeers:
		row, _ := cc.peersView.GetSelection()
		if row > 1 {
			cc.peersView.Select(row-1, 0)
		}
	case paneActivity:
		row, col := cc.activityView.GetScrollOffset()
		if row > 0 {
			cc.activityView.ScrollTo(row-1, col)
		}
	}
	cc.updateHelpBar()
}

// handleArrowDown handles down arrow based on focused pane
func (cc *ControlCenter) handleArrowDown() {
	switch cc.focusedPane {
	case paneServices:
		row, _ := cc.servicesView.GetSelection()
		rowCount := cc.servicesView.GetRowCount()
		if row < rowCount-1 {
			cc.servicesView.Select(row+1, 0)
		}
	case paneActions:
		row, _ := cc.actionsView.GetSelection()
		rowCount := cc.actionsView.GetRowCount()
		if row < rowCount-1 {
			cc.actionsView.Select(row+1, 0)
		}
	case panePeers:
		row, _ := cc.peersView.GetSelection()
		rowCount := cc.peersView.GetRowCount()
		if row < rowCount-1 {
			cc.peersView.Select(row+1, 0)
		}
	case paneActivity:
		row, col := cc.activityView.GetScrollOffset()
		cc.activityView.ScrollTo(row+1, col)
	}
	cc.updateHelpBar()
}

// toggleSelectedService starts or stops the selected service based on its current state
func (cc *ControlCenter) toggleSelectedService() {
	status := cc.getSelectedServiceStatus()
	if status == "running" {
		cc.stopSelectedService()
	} else {
		cc.startSelectedService()
	}
}

// pingSelectedPeer pings the peer selected in the peers table
func (cc *ControlCenter) pingSelectedPeer() {
	if !cc.data.Connected || len(cc.data.Peers) == 0 {
		return
	}

	row, _ := cc.peersView.GetSelection()
	if row < 1 || row > len(cc.data.Peers) {
		return
	}

	peer := cc.data.Peers[row-1]
	if peer.IP == "" {
		cc.AddActivity("warning", fmt.Sprintf("No IP for %s", peer.Hostname))
		return
	}

	go func() {
		cc.AddActivity("info", fmt.Sprintf("Pinging %s...", peer.Hostname))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		latency, connType, relay, err := network.PingPeer(ctx, peer.IP)
		if err != nil {
			cc.AddActivity("warning", fmt.Sprintf("%s: unreachable", peer.Hostname))
			return
		}

		connInfo := connType
		if relay != "" {
			connInfo = fmt.Sprintf("relay via %s", relay)
		}

		cc.AddActivity("success", fmt.Sprintf("%s: %.1fms (%s)", peer.Hostname, latency, connInfo))
	}()
}

// getSelectedServiceName returns the name of the currently selected service
func (cc *ControlCenter) getSelectedServiceName() string {
	row, _ := cc.servicesView.GetSelection()
	if row <= 0 || row > len(cc.data.Services) {
		return ""
	}
	return cc.data.Services[row-1].Name
}

// getSelectedServiceStatus returns the status of the currently selected service
func (cc *ControlCenter) getSelectedServiceStatus() string {
	row, _ := cc.servicesView.GetSelection()
	if row <= 0 || row > len(cc.data.Services) {
		return ""
	}
	return cc.data.Services[row-1].Status
}

// startSelectedService starts the currently selected service
func (cc *ControlCenter) startSelectedService() {
	svcName := cc.getSelectedServiceName()
	if svcName == "" || cc.startServiceFn == nil {
		return
	}

	go func() {
		cc.AddActivity("info", fmt.Sprintf("Starting %s...", svcName))
		if err := cc.startServiceFn(svcName); err != nil {
			cc.AddActivity("error", fmt.Sprintf("Failed to start %s: %v", svcName, err))
		} else {
			cc.AddActivity("success", fmt.Sprintf("%s started", svcName))
			cc.refresh()
		}
	}()
}

// stopSelectedService stops the currently selected service
func (cc *ControlCenter) stopSelectedService() {
	svcName := cc.getSelectedServiceName()
	if svcName == "" || cc.stopServiceFn == nil {
		return
	}

	go func() {
		cc.AddActivity("info", fmt.Sprintf("Stopping %s...", svcName))
		if err := cc.stopServiceFn(svcName); err != nil {
			cc.AddActivity("error", fmt.Sprintf("Failed to stop %s: %v", svcName, err))
		} else {
			cc.AddActivity("success", fmt.Sprintf("%s stopped", svcName))
			cc.refresh()
		}
	}()
}

// restartSelectedService restarts the currently selected service
func (cc *ControlCenter) restartSelectedService() {
	svcName := cc.getSelectedServiceName()
	if svcName == "" {
		return
	}

	// Use dedicated restart if available, otherwise stop then start
	if cc.restartServiceFn != nil {
		go func() {
			cc.AddActivity("info", fmt.Sprintf("Restarting %s...", svcName))
			if err := cc.restartServiceFn(svcName); err != nil {
				cc.AddActivity("error", fmt.Sprintf("Failed to restart %s: %v", svcName, err))
			} else {
				cc.AddActivity("success", fmt.Sprintf("%s restarted", svcName))
				cc.refresh()
			}
		}()
	} else if cc.stopServiceFn != nil && cc.startServiceFn != nil {
		go func() {
			cc.AddActivity("info", fmt.Sprintf("Restarting %s...", svcName))
			if err := cc.stopServiceFn(svcName); err != nil {
				cc.AddActivity("error", fmt.Sprintf("Failed to stop %s: %v", svcName, err))
				return
			}
			if err := cc.startServiceFn(svcName); err != nil {
				cc.AddActivity("error", fmt.Sprintf("Failed to start %s: %v", svcName, err))
			} else {
				cc.AddActivity("success", fmt.Sprintf("%s restarted", svcName))
				cc.refresh()
			}
		}()
	}
}

// showServiceLogs shows recent logs for the selected service
func (cc *ControlCenter) showServiceLogs(svcName string) {
	if cc.getServiceLogsFn == nil {
		cc.AddActivity("info", "Service logs not available")
		return
	}

	cc.inModal = true

	textView := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true)
	textView.SetBorder(true).SetTitle(fmt.Sprintf(" Logs: %s ", svcName))

	// Fetch logs
	go func() {
		logs, err := cc.getServiceLogsFn(svcName)
		cc.app.QueueUpdateDraw(func() {
			if err != nil {
				textView.SetText(fmt.Sprintf("[red]Error fetching logs:[-] %v\n\n[gray]Press Esc to close[-]", err))
			} else if len(logs) == 0 {
				textView.SetText("[gray]No logs available[-]\n\n[gray]Press Esc to close[-]")
			} else {
				var sb strings.Builder
				for _, line := range logs {
					sb.WriteString(line)
					sb.WriteString("\n")
				}
				sb.WriteString("\n[gray]Press Esc to close[-]")
				textView.SetText(sb.String())
			}
		})
	}()

	textView.SetText("[gray]Loading logs...[-]")

	textView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			cc.inModal = false
			cc.app.SetRoot(cc.mainFlex, true)
			cc.updatePaneFocus()
			return nil
		}
		return event
	})

	cc.app.SetRoot(textView, true)
	cc.app.SetFocus(textView)
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
				cc.app.SetFocus(cc.servicesView)
				cc.showInstallServiceHelp()
			default:
				cc.app.SetRoot(cc.mainFlex, true)
				cc.app.SetFocus(cc.servicesView)
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
	helpText := `[yellow::b]Citadel Control Center[-:-:-]

[yellow]Navigation:[-]
  [white::b]Tab[-:-:-]           Switch between Services/Peers panes
  [white::b]↑/↓[-:-:-] or [white::b]j/k[-:-:-]   Navigate within focused pane
  [white::b]Enter[-:-:-]         Toggle service / Ping peer

[yellow]Services Pane:[-]
  [white::b]s[-:-:-]             Start selected service
  [white::b]x[-:-:-]             Stop selected service

[yellow]Peers Pane:[-]
  [white::b]p[-:-:-]             Ping selected peer

[yellow]Actions (number keys):[-]
  [white::b]1[-:-:-]  Add Service      [white::b]2[-:-:-]  Expose Port
  [white::b]3[-:-:-]  Port Forwards    [white::b]4[-:-:-]  SSH Access
  [white::b]5[-:-:-]  Ping All Peers   [white::b]6[-:-:-]  Install Service
  [white::b]0[-:-:-]  Connect/Disconnect

[yellow]General:[-]
  [white::b]r[-:-:-]             Refresh status
  [white::b]c[-:-:-]             Copy focused panel to clipboard
  [white::b]C[-:-:-]             Copy all panels to clipboard
  [white::b]?[-:-:-] or [white::b]h[-:-:-]       Show this help
  [white::b]q[-:-:-] / [white::b]Esc[-:-:-]      Quit (with confirmation)

[gray]Press any key to close[-]`

	cc.inModal = true

	helpView := tview.NewTextView().
		SetDynamicColors(true).
		SetText(helpText)
	helpView.SetBorder(true).SetTitle(" Help ")

	helpView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		cc.inModal = false
		cc.app.SetRoot(cc.mainFlex, true)
		cc.app.SetFocus(cc.servicesView)
		return nil
	})

	cc.app.SetRoot(helpView, true)
	cc.app.SetFocus(helpView)
}

func (cc *ControlCenter) updateAllPanels() {
	cc.updateNodePanel()
	cc.updateVitalsPanel()
	cc.updateServicesView()
	cc.updateJobsPanel()
	cc.updateActionsPanel()
	cc.updatePeersView()
	cc.updateActivityView()
	cc.updateStatusBar()
}

// Action definitions for the actions table
type actionDef struct {
	key   string
	name  string
	desc  string
	fn    func()
}

func (cc *ControlCenter) getActions() []actionDef {
	connectAction := actionDef{key: "0", name: "Connect", desc: "[gray]○ Offline[-]", fn: cc.showNetworkModal}
	if cc.data.Connected {
		connectAction = actionDef{key: "0", name: "Disconnect", desc: "[green]● Connected[-]", fn: cc.showNetworkModal}
	}

	return []actionDef{
		{key: "1", name: "Add Service", desc: "[gray]vLLM, Ollama, etc.[-]", fn: cc.showAddServiceModal},
		{key: "2", name: "Expose Port", desc: "[gray]Share local port[-]", fn: cc.showExposePortModal},
		{key: "3", name: "Port Forwards", desc: "[gray]List active tunnels[-]", fn: cc.showPortForwardsModal},
		{key: "4", name: "SSH Access", desc: "[gray]Enable inbound SSH[-]", fn: cc.showSSHAccessModal},
		{key: "5", name: "Ping Peers", desc: "[gray]Test connectivity[-]", fn: cc.pingPeers},
		{key: "6", name: "Install Service", desc: "[gray]Run as system svc[-]", fn: cc.showInstallServiceModal},
		connectAction,
	}
}

func (cc *ControlCenter) updateActionsPanel() {
	cc.actionsView.Clear()

	actions := cc.getActions()
	for i, action := range actions {
		cc.actionsView.SetCell(i, 0, tview.NewTableCell("[yellow::b]"+action.key+"[-:-:-]").SetSelectable(true))
		cc.actionsView.SetCell(i, 1, tview.NewTableCell(action.name).SetSelectable(true).SetExpansion(1))
		cc.actionsView.SetCell(i, 2, tview.NewTableCell(action.desc).SetSelectable(true))
	}

	cc.actionsView.Select(0, 0)
}

func (cc *ControlCenter) updateNodePanel() {
	var sb strings.Builder

	nodeName := cc.data.NodeName
	if nodeName == "" {
		nodeName = "unknown"
	}

	statusIcon := "[red]●[-]"
	statusText := "OFFLINE"
	if cc.data.Connected {
		statusIcon = "[green]●[-]"
		statusText = "ONLINE"
	}

	sb.WriteString(fmt.Sprintf(" [yellow]Node:[-]   %s\n", nodeName))

	// Show IPs - both if dual connection
	if cc.data.DualConnection {
		sb.WriteString(fmt.Sprintf(" [yellow]Citadel:[-] [cyan]%s[-]\n", cc.data.NodeIP))
		sb.WriteString(fmt.Sprintf(" [yellow]System:[-]  [gray]%s[-]\n", cc.data.SystemTailscaleIP))
	} else if cc.data.Connected && cc.data.NodeIP != "" {
		sb.WriteString(fmt.Sprintf(" [yellow]IP:[-]     %s\n", cc.data.NodeIP))
	} else if cc.data.SystemTailscaleRunning && cc.data.SystemTailscaleIP != "" {
		sb.WriteString(fmt.Sprintf(" [yellow]TS IP:[-]  [gray]%s[-]\n", cc.data.SystemTailscaleIP))
	} else {
		sb.WriteString(" [yellow]IP:[-]     -\n")
	}

	// Show org and user info
	if cc.data.OrgID != "" {
		sb.WriteString(fmt.Sprintf(" [yellow]Org:[-]    %s\n", cc.data.OrgID))
	}
	if cc.data.UserEmail != "" {
		// Show just the email, or name if available
		userDisplay := cc.data.UserEmail
		if cc.data.UserName != "" {
			userDisplay = cc.data.UserName
		}
		sb.WriteString(fmt.Sprintf(" [yellow]User:[-]   %s\n", userDisplay))
	}

	sb.WriteString(fmt.Sprintf(" [yellow]Status:[-] %s %s\n", statusIcon, statusText))

	// Demo server URL
	if cc.data.DemoServerURL != "" {
		sb.WriteString(fmt.Sprintf(" [yellow]Demo:[-]   [cyan]%s[-]\n", cc.data.DemoServerURL))
	}

	// Terminal server URL (only shown when connected)
	if cc.data.TerminalServerURL != "" {
		sb.WriteString(fmt.Sprintf(" [yellow]Terminal:[-] [cyan]%s[-]\n", cc.data.TerminalServerURL))
	}

	// Heartbeat indicator
	if cc.data.HeartbeatActive {
		ago := time.Since(cc.data.LastHeartbeat)
		var agoStr string
		if ago < time.Minute {
			agoStr = fmt.Sprintf("%ds ago", int(ago.Seconds()))
		} else {
			agoStr = fmt.Sprintf("%dm ago", int(ago.Minutes()))
		}
		sb.WriteString(fmt.Sprintf(" [yellow]Heartbeat:[-] [green]●[-] %s", agoStr))
	} else if cc.data.WorkerRunning {
		sb.WriteString(" [yellow]Heartbeat:[-] [gray]○[-] starting...")
	}

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
	// Preserve current selection
	currentRow, _ := cc.servicesView.GetSelection()

	cc.servicesView.Clear()

	// Header
	headers := []string{"SERVICE", "STATUS", "UPTIME"}
	for i, h := range headers {
		cell := tview.NewTableCell("[yellow::b]" + h + "[-:-:-]").
			SetSelectable(false).
			SetAlign(tview.AlignLeft)
		cc.servicesView.SetCell(0, i, cell)
	}

	row := 1
	if len(cc.data.Services) == 0 && len(cc.activeForwards) == 0 {
		cc.servicesView.SetCell(1, 0, tview.NewTableCell("[gray]No services configured[-]").SetSelectable(false))
		return
	}

	// Services
	for _, svc := range cc.data.Services {
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
		row++
	}

	// Exposed ports section
	if len(cc.activeForwards) > 0 {
		// Separator
		cc.servicesView.SetCell(row, 0, tview.NewTableCell("[yellow::b]─── EXPOSED ───[-:-:-]").SetSelectable(false))
		cc.servicesView.SetCell(row, 1, tview.NewTableCell("").SetSelectable(false))
		cc.servicesView.SetCell(row, 2, tview.NewTableCell("").SetSelectable(false))
		row++

		for _, fwd := range cc.activeForwards {
			desc := fwd.Description
			if desc == "" {
				desc = "port"
			}
			cc.servicesView.SetCell(row, 0, tview.NewTableCell(fmt.Sprintf(" :%d", fwd.LocalPort)).SetSelectable(true))
			cc.servicesView.SetCell(row, 1, tview.NewTableCell("[cyan]● exposed[-]").SetSelectable(true))
			cc.servicesView.SetCell(row, 2, tview.NewTableCell("[gray]"+desc+"[-]").SetSelectable(true))
			row++
		}
	}

	// Restore selection (or default to first row if invalid)
	totalRows := len(cc.data.Services) + len(cc.activeForwards)
	if len(cc.activeForwards) > 0 {
		totalRows++ // account for separator
	}
	if currentRow < 1 || currentRow > totalRows {
		currentRow = 1
	}
	cc.servicesView.Select(currentRow, 0)
}

func (cc *ControlCenter) updateJobsPanel() {
	var sb strings.Builder

	// Worker status - prominent at top
	if cc.data.WorkerRunning {
		sb.WriteString(" [green::b]● WORKER ACTIVE[-:-:-]\n")
	} else {
		sb.WriteString(" [gray]○ Worker stopped[-]\n")
	}

	// Queue subscription - compact
	if cc.data.WorkerQueue != "" {
		sb.WriteString(fmt.Sprintf(" [yellow]Queue:[-] %s\n", cc.data.WorkerQueue))
	}

	// Job stats - compact summary
	sb.WriteString(fmt.Sprintf(" [yellow]Jobs:[-] %d done", cc.data.Jobs.Completed))
	if cc.data.Jobs.Pending > 0 {
		sb.WriteString(fmt.Sprintf(", %d pending", cc.data.Jobs.Pending))
	}
	if cc.data.Jobs.Failed > 0 {
		sb.WriteString(fmt.Sprintf(", [red]%d failed[-]", cc.data.Jobs.Failed))
	}
	sb.WriteString("\n")

	// Recent jobs - last 3 (for compact panel view)
	cc.recentJobsMu.Lock()
	recentJobs := cc.recentJobs
	cc.recentJobsMu.Unlock()

	if len(recentJobs) > 0 {
		sb.WriteString("\n [yellow]Recent:[-]\n")
		for i, job := range recentJobs {
			if i >= 3 {
				break
			}
			statusIcon := "[green]✓[-]"
			if job.Status == "failed" {
				statusIcon = "[red]✗[-]"
			} else if job.Status == "processing" {
				statusIcon = "[cyan]●[-]"
			}
			sb.WriteString(fmt.Sprintf(" %s %s [gray]%s[-]\n", statusIcon, job.Type, formatDurationCompact(job.Duration)))
		}
	}

	cc.jobsPanel.SetText(sb.String())
}

// formatDurationCompact formats duration in compact form like "1.2s" or "45ms"
func formatDurationCompact(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fm", d.Minutes())
}

func (cc *ControlCenter) updatePeersView() {
	cc.peersView.Clear()

	// Header row (fixed, not selectable)
	headers := []string{" ", "HOSTNAME", "IP", "STATUS", "LATENCY"}
	for i, h := range headers {
		cell := tview.NewTableCell("[yellow::b]" + h + "[-:-:-]").
			SetSelectable(false).
			SetExpansion(1)
		if i == 0 {
			cell.SetExpansion(0) // Icon column fixed width
		}
		cc.peersView.SetCell(0, i, cell)
	}

	if !cc.data.Connected {
		// Show disconnected message
		cell := tview.NewTableCell(" [gray]Not connected - press [yellow]0[-] [gray]to connect[-]").
			SetSelectable(false)
		cc.peersView.SetCell(1, 0, cell)
		return
	}

	if len(cc.data.Peers) == 0 {
		cell := tview.NewTableCell(" [gray]No other peers on network[-]").
			SetSelectable(false)
		cc.peersView.SetCell(1, 0, cell)
		return
	}

	// Peer rows
	for i, peer := range cc.data.Peers {
		row := i + 1 // Start after header

		// Status icon
		icon := "[gray]○[-]"
		statusText := "[gray]offline[-]"
		if peer.Online {
			icon = "[green]●[-]"
			statusText = "[green]online[-]"
		}

		cc.peersView.SetCell(row, 0, tview.NewTableCell(icon).SetSelectable(true))
		cc.peersView.SetCell(row, 1, tview.NewTableCell(peer.Hostname).SetSelectable(true))
		cc.peersView.SetCell(row, 2, tview.NewTableCell("[gray]"+peer.IP+"[-]").SetSelectable(true))
		cc.peersView.SetCell(row, 3, tview.NewTableCell(statusText).SetSelectable(true))

		latency := peer.Latency
		if latency == "" {
			latency = "-"
		}
		cc.peersView.SetCell(row, 4, tview.NewTableCell("[gray]"+latency+"[-]").SetSelectable(true))
	}

	// Select first data row if available
	if len(cc.data.Peers) > 0 {
		cc.peersView.Select(1, 0)
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
	switch cc.focusedPane {
	case paneNode:
		cc.helpBar.SetText("[yellow::b]Enter[-:-:-] details  │  [yellow::b]Tab[-:-:-] switch pane  [yellow::b]c[-:-:-] copy  [yellow::b]?[-:-:-] help  [yellow::b]q[-:-:-] quit")
	case paneSystem:
		cc.helpBar.SetText("[yellow::b]Enter[-:-:-] details  │  [yellow::b]Tab[-:-:-] switch pane  [yellow::b]c[-:-:-] copy  [yellow::b]?[-:-:-] help  [yellow::b]q[-:-:-] quit")
	case paneJobs:
		cc.helpBar.SetText("[yellow::b]Enter[-:-:-] view details  │  [yellow::b]Tab[-:-:-] switch pane  [yellow::b]c[-:-:-] copy  [yellow::b]?[-:-:-] help  [yellow::b]q[-:-:-] quit")
	case paneServices:
		svcName := cc.getSelectedServiceName()
		svcStatus := cc.getSelectedServiceStatus()
		if svcName != "" {
			statusIcon := "[gray]○[-]"
			action := "start"
			if svcStatus == "running" {
				statusIcon = "[green]●[-]"
				action = "stop"
			}
			cc.helpBar.SetText(fmt.Sprintf("[white::b]%s[-:-:-] %s  │  [yellow::b]Enter[-:-:-] %s  [yellow::b]Tab[-:-:-] switch pane  [yellow::b]0-7[-:-:-] actions  [yellow::b]?[-:-:-] help",
				svcName, statusIcon, action))
		} else {
			cc.helpBar.SetText("[yellow::b]↑/↓[-:-:-] select  │  [yellow::b]Tab[-:-:-] switch pane  [yellow::b]0-7[-:-:-] actions  [yellow::b]?[-:-:-] help  [yellow::b]q[-:-:-] quit")
		}
	case paneActions:
		row, _ := cc.actionsView.GetSelection()
		actions := cc.getActions()
		if row >= 0 && row < len(actions) {
			action := actions[row]
			cc.helpBar.SetText(fmt.Sprintf("[yellow::b]%s[-:-:-] [white::b]%s[-:-:-]  │  [yellow::b]Enter[-:-:-] execute  [yellow::b]Tab[-:-:-] switch pane  [yellow::b]?[-:-:-] help  [yellow::b]q[-:-:-] quit",
				action.key, action.name))
		} else {
			cc.helpBar.SetText("[yellow::b]↑/↓[-:-:-] select action  │  [yellow::b]Enter[-:-:-] execute  [yellow::b]Tab[-:-:-] switch pane  [yellow::b]?[-:-:-] help")
		}
	case panePeers:
		if len(cc.data.Peers) > 0 {
			row, _ := cc.peersView.GetSelection()
			if row > 0 && row <= len(cc.data.Peers) {
				peer := cc.data.Peers[row-1]
				cc.helpBar.SetText(fmt.Sprintf("[white::b]%s[-:-:-]  │  [yellow::b]Enter[-:-:-] view peers  [yellow::b]p[-:-:-] quick ping  [yellow::b]Tab[-:-:-] switch  [yellow::b]?[-:-:-] help",
					peer.Hostname))
			} else {
				cc.helpBar.SetText("[yellow::b]↑/↓[-:-:-] select peer  │  [yellow::b]Enter[-:-:-] view peers  [yellow::b]Tab[-:-:-] switch pane  [yellow::b]?[-:-:-] help")
			}
		} else {
			cc.helpBar.SetText("[yellow::b]Tab[-:-:-] switch pane  │  [yellow::b]0[-:-:-] connect  [yellow::b]?[-:-:-] help  [yellow::b]q[-:-:-] quit")
		}
	case paneActivity:
		cc.helpBar.SetText("[yellow::b]Enter[-:-:-] full screen  │  [yellow::b]l[-:-:-] copy logs  [yellow::b]↑/↓[-:-:-] scroll  [yellow::b]Tab[-:-:-] switch  [yellow::b]?[-:-:-] help")
	}
}

func (cc *ControlCenter) updateStatusBar() {
	lastRefreshStr := "[gray]starting...[-]"
	if !cc.lastRefresh.IsZero() {
		lastRefreshStr = "[gray]" + cc.lastRefresh.Format("15:04:05") + "[-]"
	}

	cc.statusBar.SetText(fmt.Sprintf("Refresh: [green]auto (30s)[-]  │  Last: %s  │  Press [yellow::b]?[-:-:-] for help", lastRefreshStr))
}

// showSuggestion displays a context-aware suggestion that auto-dismisses
func (cc *ControlCenter) showSuggestion() {
	suggestion := cc.getContextualSuggestion()
	if suggestion == "" {
		cc.suggestionBar.SetText("")
		return
	}

	cc.showingSuggestion = true
	cc.suggestionBar.SetText(fmt.Sprintf("[yellow]Tip:[-] %s  [gray](press any key to dismiss)[-]", suggestion))

	// Auto-dismiss after 10 seconds
	if cc.suggestionTimer != nil {
		cc.suggestionTimer.Stop()
	}
	cc.suggestionTimer = time.AfterFunc(10*time.Second, func() {
		cc.app.QueueUpdateDraw(func() {
			cc.dismissSuggestion()
		})
	})
}

// dismissSuggestion hides the suggestion bar
func (cc *ControlCenter) dismissSuggestion() {
	if !cc.showingSuggestion {
		return
	}
	cc.showingSuggestion = false
	cc.suggestionBar.SetText("")
	if cc.suggestionTimer != nil {
		cc.suggestionTimer.Stop()
		cc.suggestionTimer = nil
	}
}

// getContextualSuggestion returns a context-aware suggestion based on current state
func (cc *ControlCenter) getContextualSuggestion() string {
	// Not connected - suggest connecting
	if !cc.data.Connected && !cc.data.SystemTailscaleRunning {
		return "Press [yellow::b]0[-:-:-] to connect to AceTeam Network"
	}

	// No services configured - suggest adding one
	if len(cc.data.Services) == 0 {
		return "Press [yellow::b]1[-:-:-] to add your first service (Ollama, vLLM, etc.)"
	}

	// All services stopped - suggest starting one
	allStopped := true
	for _, svc := range cc.data.Services {
		if svc.Status == "running" {
			allStopped = false
			break
		}
	}
	if allStopped && len(cc.data.Services) > 0 {
		return "Select a service and press [yellow::b]Enter[-:-:-] to manage it"
	}

	// Worker not running but connected - suggest starting worker
	if cc.data.Connected && !cc.data.WorkerRunning {
		return "Worker not running - jobs won't be processed on this node"
	}

	return ""
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

// UpdateHeartbeat updates the heartbeat status (thread-safe)
func (cc *ControlCenter) UpdateHeartbeat(active bool) {
	cc.data.HeartbeatActive = active
	cc.data.LastHeartbeat = time.Now()
	if cc.app != nil && cc.nodePanel != nil && cc.running {
		cc.app.QueueUpdateDraw(func() {
			cc.updateNodePanel()
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

// RecordJob records a job completion for history tracking
func (cc *ControlCenter) RecordJob(record JobRecord) {
	cc.recentJobsMu.Lock()
	defer cc.recentJobsMu.Unlock()

	// Prepend the new job (newest first)
	cc.recentJobs = append([]JobRecord{record}, cc.recentJobs...)

	// Keep only last 10 jobs
	if len(cc.recentJobs) > 10 {
		cc.recentJobs = cc.recentJobs[:10]
	}

	// Also update the data copy for display
	cc.data.RecentJobs = cc.recentJobs

	// Update UI
	if cc.app != nil && cc.jobsPanel != nil && cc.running {
		go func() {
			cc.app.QueueUpdateDraw(func() {
				cc.updateJobsPanel()
			})
		}()
	}
}

// GetRecentJobs returns a copy of recent jobs
func (cc *ControlCenter) GetRecentJobs() []JobRecord {
	cc.recentJobsMu.Lock()
	defer cc.recentJobsMu.Unlock()
	result := make([]JobRecord, len(cc.recentJobs))
	copy(result, cc.recentJobs)
	return result
}

// copyFocusedPanel copies the content of the focused panel to clipboard/file
func (cc *ControlCenter) copyFocusedPanel() {
	var content string
	var paneName string

	switch cc.focusedPane {
	case paneServices:
		paneName = "Services"
		content = cc.getServicesText()
	case paneActions:
		paneName = "Actions"
		content = cc.getActionsText()
	case panePeers:
		paneName = "Peers"
		content = cc.getPeersText()
	case paneActivity:
		paneName = "Activity"
		content = cc.getActivityText()
	}

	if err := cc.copyToClipboard(content); err != nil {
		// Fall back to file
		filePath := "/tmp/citadel-panel.txt"
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			cc.AddActivity("error", fmt.Sprintf("Failed to copy: %v", err))
			return
		}
		cc.AddActivity("success", fmt.Sprintf("%s copied to %s", paneName, filePath))
	} else {
		cc.AddActivity("success", fmt.Sprintf("%s copied to clipboard", paneName))
	}
}

// copyAllPanels copies all panel content to clipboard/file
func (cc *ControlCenter) copyAllPanels() {
	var sb strings.Builder

	sb.WriteString("=== CITADEL CONTROL CENTER ===\n")
	sb.WriteString(fmt.Sprintf("Timestamp: %s\n\n", time.Now().Format("2006-01-02 15:04:05")))

	sb.WriteString("--- Node ---\n")
	sb.WriteString(cc.getNodeText())
	sb.WriteString("\n\n--- System ---\n")
	sb.WriteString(cc.getVitalsText())
	sb.WriteString("\n\n--- Jobs ---\n")
	sb.WriteString(cc.getJobsText())
	sb.WriteString("\n\n--- Services ---\n")
	sb.WriteString(cc.getServicesText())
	sb.WriteString("\n\n--- Peers ---\n")
	sb.WriteString(cc.getPeersText())
	sb.WriteString("\n\n--- Activity ---\n")
	sb.WriteString(cc.getActivityText())

	content := sb.String()

	if err := cc.copyToClipboard(content); err != nil {
		// Fall back to file
		filePath := "/tmp/citadel-panels.txt"
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			cc.AddActivity("error", fmt.Sprintf("Failed to copy: %v", err))
			return
		}
		cc.AddActivity("success", fmt.Sprintf("All panels copied to %s", filePath))
	} else {
		cc.AddActivity("success", "All panels copied to clipboard")
	}
}

// copyToClipboard attempts to copy text to the system clipboard
func (cc *ControlCenter) copyToClipboard(text string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "linux":
		// Try xclip first, then xsel
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		} else if _, err := exec.LookPath("xsel"); err == nil {
			cmd = exec.Command("xsel", "--clipboard", "--input")
		} else {
			return fmt.Errorf("no clipboard tool found (install xclip or xsel)")
		}
	case "windows":
		cmd = exec.Command("clip")
	default:
		return fmt.Errorf("clipboard not supported on %s", runtime.GOOS)
	}

	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

// getServicesText returns plain text representation of services
func (cc *ControlCenter) getServicesText() string {
	var sb strings.Builder
	sb.WriteString("SERVICE\t\tSTATUS\t\tUPTIME\n")
	for _, svc := range cc.data.Services {
		uptime := svc.Uptime
		if uptime == "" {
			uptime = "-"
		}
		sb.WriteString(fmt.Sprintf("%s\t\t%s\t\t%s\n", svc.Name, svc.Status, uptime))
	}
	if len(cc.data.Services) == 0 {
		sb.WriteString("(no services configured)\n")
	}
	return sb.String()
}

// getActionsText returns plain text representation of actions
func (cc *ControlCenter) getActionsText() string {
	var sb strings.Builder
	actions := cc.getActions()
	for _, a := range actions {
		sb.WriteString(fmt.Sprintf("[%s] %s\n", a.key, a.name))
	}
	return sb.String()
}

// getPeersText returns plain text representation of peers
func (cc *ControlCenter) getPeersText() string {
	var sb strings.Builder
	if !cc.data.Connected {
		sb.WriteString("(not connected)\n")
		return sb.String()
	}
	sb.WriteString("HOSTNAME\t\tIP\t\tSTATUS\t\tLATENCY\n")
	for _, peer := range cc.data.Peers {
		status := "offline"
		if peer.Online {
			status = "online"
		}
		latency := peer.Latency
		if latency == "" {
			latency = "-"
		}
		sb.WriteString(fmt.Sprintf("%s\t\t%s\t\t%s\t\t%s\n", peer.Hostname, peer.IP, status, latency))
	}
	if len(cc.data.Peers) == 0 {
		sb.WriteString("(no peers)\n")
	}
	return sb.String()
}

// getActivityText returns plain text representation of activity log
func (cc *ControlCenter) getActivityText() string {
	cc.activityMu.Lock()
	defer cc.activityMu.Unlock()

	var sb strings.Builder
	for _, entry := range cc.activities {
		sb.WriteString(fmt.Sprintf("%s [%s] %s\n", entry.Time.Format("15:04:05"), entry.Level, entry.Message))
	}
	if len(cc.activities) == 0 {
		sb.WriteString("(no activity)\n")
	}
	return sb.String()
}

// showActivityFullScreen shows activity log in a full screen modal
func (cc *ControlCenter) showActivityFullScreen() {
	cc.inModal = true

	textView := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true)

	// Build content
	cc.activityMu.Lock()
	var sb strings.Builder
	sb.WriteString("[yellow::b]Activity Log[-:-:-]\n\n")
	sb.WriteString(fmt.Sprintf("[gray]Session logs are kept in memory (up to 100 entries)[-]\n"))
	sb.WriteString(fmt.Sprintf("[gray]Press 'l' to copy logs to /tmp/citadel-activity.log[-]\n\n"))

	if len(cc.activities) == 0 {
		sb.WriteString("[gray]No activity yet[-]\n")
	} else {
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

			sb.WriteString(fmt.Sprintf("[gray]%s[-] [%s]%s[-] %s\n", timeStr, color, icon, entry.Message))
		}
	}
	cc.activityMu.Unlock()

	sb.WriteString("\n[gray]Press Esc to close, 'l' to copy logs[-]")
	textView.SetText(sb.String())
	textView.SetBorder(true).SetTitle(" Activity Log (Full Screen) ")

	textView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			cc.inModal = false
			cc.app.SetRoot(cc.mainFlex, true)
			cc.updatePaneFocus()
			return nil
		case tcell.KeyRune:
			if event.Rune() == 'l' || event.Rune() == 'L' {
				cc.copyActivityLogs()
				return nil
			}
		}
		return event
	})

	cc.app.SetRoot(textView, true)
	cc.app.SetFocus(textView)
}

// copyActivityLogs copies recent log lines to clipboard
func (cc *ControlCenter) copyActivityLogs() {
	var content string
	var lineCount int

	// Try to read from log file first
	homeDir, err := os.UserHomeDir()
	if err == nil {
		logPath := homeDir + "/.citadel-cli/logs/latest.log"
		if data, err := os.ReadFile(logPath); err == nil && len(data) > 0 {
			// Get last 50 lines
			lines := strings.Split(string(data), "\n")
			start := len(lines) - 50
			if start < 0 {
				start = 0
			}
			lineCount = len(lines) - start
			content = strings.Join(lines[start:], "\n")
		}
	}

	// Fall back to activity log if no log file
	if content == "" {
		cc.activityMu.Lock()
		var sb strings.Builder
		// Get up to 50 entries (activity log is already newest-first)
		count := len(cc.activities)
		if count > 50 {
			count = 50
		}
		lineCount = count
		for i := count - 1; i >= 0; i-- {
			entry := cc.activities[i]
			sb.WriteString(fmt.Sprintf("%s [%s] %s\n", entry.Time.Format("15:04:05"), entry.Level, entry.Message))
		}
		cc.activityMu.Unlock()
		content = sb.String()
	}

	if content == "" {
		cc.AddActivity("info", "No logs to copy")
		return
	}

	// Copy to clipboard
	if err := cc.copyToClipboard(content); err != nil {
		// Fall back to file
		filePath := "/tmp/citadel-logs.txt"
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			cc.AddActivity("error", fmt.Sprintf("Failed to copy logs: %v", err))
			return
		}
		cc.AddActivity("success", fmt.Sprintf("%d lines saved to %s", lineCount, filePath))
	} else {
		cc.AddActivity("success", fmt.Sprintf("%d lines copied to clipboard", lineCount))
	}
}

// getNodeText returns plain text representation of node info
func (cc *ControlCenter) getNodeText() string {
	var sb strings.Builder
	nodeName := cc.data.NodeName
	if nodeName == "" {
		nodeName = "unknown"
	}
	nodeIP := cc.data.NodeIP
	if nodeIP == "" {
		nodeIP = "-"
	}
	status := "OFFLINE"
	if cc.data.Connected {
		status = "ONLINE"
	}
	sb.WriteString(fmt.Sprintf("Node:   %s\n", nodeName))
	sb.WriteString(fmt.Sprintf("IP:     %s\n", nodeIP))
	if cc.data.OrgID != "" {
		sb.WriteString(fmt.Sprintf("Org:    %s\n", cc.data.OrgID))
	}
	sb.WriteString(fmt.Sprintf("Status: %s\n", status))
	return sb.String()
}

// getVitalsText returns plain text representation of system vitals
func (cc *ControlCenter) getVitalsText() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("CPU:  %.1f%%\n", cc.data.CPUPercent))
	sb.WriteString(fmt.Sprintf("Mem:  %.1f%% (%s / %s)\n", cc.data.MemoryPercent, cc.data.MemoryUsed, cc.data.MemoryTotal))
	sb.WriteString(fmt.Sprintf("Disk: %.1f%% (%s / %s)\n", cc.data.DiskPercent, cc.data.DiskUsed, cc.data.DiskTotal))
	if cc.data.GPUName != "" {
		sb.WriteString(fmt.Sprintf("GPU:  %s - %.1f%% (%s, %s)\n", cc.data.GPUName, cc.data.GPUUtilization, cc.data.GPUMemory, cc.data.GPUTemp))
	}
	return sb.String()
}

// getJobsText returns plain text representation of jobs panel
func (cc *ControlCenter) getJobsText() string {
	var sb strings.Builder
	if cc.data.WorkerRunning {
		sb.WriteString("Worker: ACTIVE\n")
	} else {
		sb.WriteString("Worker: stopped\n")
	}
	if cc.data.WorkerQueue != "" {
		sb.WriteString(fmt.Sprintf("Queue:  %s\n", cc.data.WorkerQueue))
	}
	sb.WriteString(fmt.Sprintf("Pending:    %d\n", cc.data.Jobs.Pending))
	sb.WriteString(fmt.Sprintf("Processing: %d\n", cc.data.Jobs.Processing))
	sb.WriteString(fmt.Sprintf("Completed:  %d\n", cc.data.Jobs.Completed))
	if cc.data.Jobs.Failed > 0 {
		sb.WriteString(fmt.Sprintf("Failed:     %d\n", cc.data.Jobs.Failed))
	}

	// Recent jobs
	cc.recentJobsMu.Lock()
	recentJobs := cc.recentJobs
	cc.recentJobsMu.Unlock()

	if len(recentJobs) > 0 {
		sb.WriteString("\nRecent Jobs:\n")
		for _, job := range recentJobs {
			durationStr := "-"
			if job.Duration > 0 {
				durationStr = job.Duration.String()
			}
			sb.WriteString(fmt.Sprintf("  %s  %s  %s  %s\n",
				job.StartedAt.Format("15:04:05"), job.Type, job.Status, durationStr))
			if job.Error != "" {
				sb.WriteString(fmt.Sprintf("    Error: %s\n", job.Error))
			}
		}
	}

	return sb.String()
}
