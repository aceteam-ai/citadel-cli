package dashboard

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/aceteam-ai/citadel-cli/internal/tui"
)

// StatusData holds all the data for the status dashboard
type StatusData struct {
	NodeName   string
	NodeIP     string
	OrgID      string
	Tags       []string
	Connected  bool
	Version    string
	LastUpdate time.Time

	// System vitals
	CPUPercent    float64
	MemoryPercent float64
	MemoryUsed    string
	MemoryTotal   string
	DiskPercent   float64
	DiskUsed      string
	DiskTotal     string

	// GPU info
	GPUs []GPUInfo

	// Services
	Services []ServiceStatus

	// Peers
	Peers []PeerInfo

	// Job queue (optional)
	JobQueueEnabled bool
	JobChannel      string
	PendingJobs     int64
	InProgressJobs  int64
	FailedJobs      int64
}

// GPUInfo holds GPU information
type GPUInfo struct {
	Name        string
	Memory      string
	Temperature string
	Utilization float64
	Driver      string
}

// StatusModel is the BubbleTea model for the interactive status dashboard
type StatusModel struct {
	data         StatusData
	width        int
	height       int
	refreshTimer time.Time
	autoRefresh  bool
	err          error
	loading      bool

	// Callback to refresh data
	refreshFn func() (StatusData, error)
}

// RefreshMsg triggers a data refresh
type RefreshMsg struct{}

// DataMsg carries refreshed data
type DataMsg struct {
	Data StatusData
	Err  error
}

// NewStatusModel creates a new status dashboard model
func NewStatusModel(data StatusData, refreshFn func() (StatusData, error)) StatusModel {
	return StatusModel{
		data:        data,
		autoRefresh: true,
		refreshFn:   refreshFn,
	}
}

func (m StatusModel) Init() tea.Cmd {
	return tea.Batch(
		tea.EnterAltScreen,
		refreshCmd(m.refreshFn),
	)
}

func refreshCmd(fn func() (StatusData, error)) tea.Cmd {
	return func() tea.Msg {
		if fn == nil {
			return DataMsg{}
		}
		data, err := fn()
		return DataMsg{Data: data, Err: err}
	}
}

func autoRefreshCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
		return RefreshMsg{}
	})
}

func (m StatusModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			// Manual refresh
			m.loading = true
			return m, refreshCmd(m.refreshFn)
		case "a":
			// Toggle auto-refresh
			m.autoRefresh = !m.autoRefresh
			if m.autoRefresh {
				return m, autoRefreshCmd()
			}
			return m, nil
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case RefreshMsg:
		if m.autoRefresh {
			m.loading = true
			return m, tea.Batch(
				refreshCmd(m.refreshFn),
				autoRefreshCmd(),
			)
		}
		return m, nil

	case DataMsg:
		m.loading = false
		if msg.Err != nil {
			m.err = msg.Err
		} else {
			m.data = msg.Data
			m.refreshTimer = time.Now()
			m.err = nil
		}
		if m.autoRefresh {
			return m, autoRefreshCmd()
		}
		return m, nil
	}

	return m, nil
}

func (m StatusModel) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	var sections []string

	// Title bar
	title := m.renderTitle()
	sections = append(sections, title)

	// Main content area
	leftCol := m.renderLeftColumn()
	rightCol := m.renderRightColumn()

	// Join columns side by side
	mainContent := lipgloss.JoinHorizontal(lipgloss.Top, leftCol, "  ", rightCol)
	sections = append(sections, mainContent)

	// Services panel (full width)
	if len(m.data.Services) > 0 {
		servicesPanel := ServicePanel{
			Title:    "SERVICES",
			Services: m.data.Services,
			Width:    m.width - 4,
		}
		sections = append(sections, servicesPanel.Render())
	}

	// Help bar
	helpItems := []HelpItem{
		{Key: "r", Label: "efresh"},
		{Key: "a", Label: "uto-refresh: " + boolStr(m.autoRefresh)},
		{Key: "q", Label: "uit"},
	}
	helpBar := HelpBar(helpItems)
	sections = append(sections, "\n"+helpBar)

	// Status line
	statusLine := m.renderStatusLine()
	sections = append(sections, statusLine)

	return strings.Join(sections, "\n")
}

func (m StatusModel) renderTitle() string {
	title := fmt.Sprintf("CITADEL STATUS (%s)", m.data.Version)
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(tui.ColorPrimary).
		Width(m.width).
		Align(lipgloss.Center)

	return titleStyle.Render(title)
}

func (m StatusModel) renderLeftColumn() string {
	halfWidth := (m.width - 6) / 2

	// Node info panel
	nodeItems := []KeyValue{
		{Key: "Node", Value: m.data.NodeName},
		{Key: "IP", Value: m.data.NodeIP},
	}
	if m.data.OrgID != "" {
		nodeItems = append(nodeItems, KeyValue{Key: "Organization", Value: m.data.OrgID})
	}
	if len(m.data.Tags) > 0 {
		nodeItems = append(nodeItems, KeyValue{Key: "Tags", Value: strings.Join(m.data.Tags, ", ")})
	}

	statusVal := "ONLINE"
	statusStyle := tui.SuccessStyle
	if !m.data.Connected {
		statusVal = "OFFLINE"
		statusStyle = tui.ErrorStyle
	}
	nodeItems = append(nodeItems, KeyValue{Key: "Status", Value: statusVal, Style: statusStyle})

	nodePanel := KeyValuePanel{
		Title: "NODE",
		Items: nodeItems,
		Width: halfWidth,
	}

	// System vitals panel
	vitalsItems := []ProgressItem{
		{Label: "CPU", Percent: m.data.CPUPercent},
		{Label: "Memory", Percent: m.data.MemoryPercent, Detail: fmt.Sprintf("%s / %s", m.data.MemoryUsed, m.data.MemoryTotal)},
		{Label: "Disk", Percent: m.data.DiskPercent, Detail: fmt.Sprintf("%s / %s", m.data.DiskUsed, m.data.DiskTotal)},
	}

	vitalsPanel := ProgressPanel{
		Title: "SYSTEM VITALS",
		Items: vitalsItems,
		Width: halfWidth,
	}

	return lipgloss.JoinVertical(lipgloss.Left, nodePanel.Render(), vitalsPanel.Render())
}

func (m StatusModel) renderRightColumn() string {
	halfWidth := (m.width - 6) / 2

	// GPU panel
	var gpuContent string
	if len(m.data.GPUs) == 0 {
		gpuContent = tui.MutedStyle.Render("No GPU detected")
	} else {
		var lines []string
		for i, gpu := range m.data.GPUs {
			lines = append(lines, tui.LabelStyle.Render(fmt.Sprintf("GPU %d:", i))+" "+gpu.Name)
			if gpu.Memory != "" {
				lines = append(lines, "  "+tui.MutedStyle.Render("Memory:")+" "+gpu.Memory)
			}
			if gpu.Temperature != "" {
				lines = append(lines, "  "+tui.MutedStyle.Render("Temp:")+" "+gpu.Temperature)
			}
			if gpu.Utilization > 0 {
				bar := tui.ProgressBar(gpu.Utilization, 15)
				pct := fmt.Sprintf("%.0f%%", gpu.Utilization)
				lines = append(lines, "  "+tui.MutedStyle.Render("Util:")+" "+bar+" "+pct)
			}
		}
		gpuContent = strings.Join(lines, "\n")
	}
	gpuPanel := Panel{
		Title:   "GPU",
		Content: gpuContent,
		Width:   halfWidth,
	}

	// Peers panel
	peerPanel := PeerPanel{
		Title: "NETWORK PEERS",
		Peers: m.data.Peers,
		Width: halfWidth,
	}

	return lipgloss.JoinVertical(lipgloss.Left, gpuPanel.Render(), peerPanel.Render())
}

func (m StatusModel) renderStatusLine() string {
	var parts []string

	if m.loading {
		parts = append(parts, tui.SpinnerStyle.Render("Refreshing..."))
	} else if m.err != nil {
		parts = append(parts, tui.ErrorStyle.Render("Error: "+m.err.Error()))
	} else {
		lastUpdate := "never"
		if !m.refreshTimer.IsZero() {
			lastUpdate = m.refreshTimer.Format("15:04:05")
		}
		parts = append(parts, tui.MutedStyle.Render("Last update: "+lastUpdate))
	}

	return strings.Join(parts, "  ")
}

func boolStr(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// RunDashboard runs the interactive status dashboard
func RunDashboard(data StatusData, refreshFn func() (StatusData, error)) error {
	model := NewStatusModel(data, refreshFn)
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err := p.Run()
	return err
}
