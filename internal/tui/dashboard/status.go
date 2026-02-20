package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/aceteam-ai/citadel-cli/internal/tui"
)

// StatusData holds all the data for the status dashboard
type StatusData struct {
	NodeName   string    `json:"nodeName"`
	NodeIP     string    `json:"nodeIP"`
	OrgID      string    `json:"orgID,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	Connected  bool      `json:"connected"`
	Version    string    `json:"version"`
	LastUpdate time.Time `json:"lastUpdate"`

	// System vitals
	CPUPercent    float64 `json:"cpuPercent"`
	MemoryPercent float64 `json:"memoryPercent"`
	MemoryUsed    string  `json:"memoryUsed"`
	MemoryTotal   string  `json:"memoryTotal"`
	DiskPercent   float64 `json:"diskPercent"`
	DiskUsed      string  `json:"diskUsed"`
	DiskTotal     string  `json:"diskTotal"`

	// GPU info
	GPUs []GPUInfo `json:"gpus,omitempty"`

	// Services
	Services []ServiceStatus `json:"services,omitempty"`

	// Peers
	Peers []PeerInfo `json:"peers,omitempty"`

	// Job queue (optional)
	JobQueueEnabled bool   `json:"jobQueueEnabled,omitempty"`
	JobChannel      string `json:"jobChannel,omitempty"`
	PendingJobs     int64  `json:"pendingJobs,omitempty"`
	InProgressJobs  int64  `json:"inProgressJobs,omitempty"`
	FailedJobs      int64  `json:"failedJobs,omitempty"`
}

// GPUInfo holds GPU information
type GPUInfo struct {
	Name        string  `json:"name"`
	Memory      string  `json:"memory"`
	Temperature string  `json:"temperature,omitempty"`
	Utilization float64 `json:"utilization"`
	Driver      string  `json:"driver,omitempty"`
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
	spinner      spinner.Model

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
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(tui.ColorPrimary)

	return StatusModel{
		data:        data,
		autoRefresh: true,
		refreshFn:   refreshFn,
		spinner:     s,
	}
}

func (m StatusModel) Init() tea.Cmd {
	return tea.Batch(
		tea.EnterAltScreen,
		m.spinner.Tick,
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
			m.loading = true
			return m, refreshCmd(m.refreshFn)
		case "a":
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

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m StatusModel) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	// Calculate box width (constrained to reasonable size)
	boxWidth := m.width - 4
	if boxWidth > 70 {
		boxWidth = 70
	}
	if boxWidth < 50 {
		boxWidth = 50
	}

	var sb strings.Builder

	// ┌─────────────────── CITADEL STATUS ────────────────────┐
	title := " CITADEL STATUS "
	titlePadding := (boxWidth - 2 - len(title)) / 2
	sb.WriteString(lipgloss.NewStyle().Foreground(tui.ColorBorder).Render("┌"))
	sb.WriteString(lipgloss.NewStyle().Foreground(tui.ColorBorder).Render(strings.Repeat("─", titlePadding)))
	sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary).Render(title))
	sb.WriteString(lipgloss.NewStyle().Foreground(tui.ColorBorder).Render(strings.Repeat("─", boxWidth-2-titlePadding-len(title))))
	sb.WriteString(lipgloss.NewStyle().Foreground(tui.ColorBorder).Render("┐"))
	sb.WriteString("\n")

	// Node info row
	nodeInfo := m.formatNodeLine(boxWidth - 4)
	sb.WriteString(m.boxLine(nodeInfo, boxWidth))

	statusInfo := m.formatStatusLine(boxWidth - 4)
	sb.WriteString(m.boxLine(statusInfo, boxWidth))

	// Divider
	sb.WriteString(m.divider(boxWidth))

	// SYSTEM VITALS header
	sb.WriteString(m.boxLine(lipgloss.NewStyle().Bold(true).Foreground(tui.ColorSecondary).Render("SYSTEM VITALS"), boxWidth))

	// Vitals rows (2x2 grid)
	vitalsRow1 := m.formatVitalsRow("CPU", m.data.CPUPercent, "Memory", m.data.MemoryPercent, boxWidth-4)
	sb.WriteString(m.boxLine(vitalsRow1, boxWidth))

	// GPU utilization or disk
	var vitalsRow2 string
	if len(m.data.GPUs) > 0 && m.data.GPUs[0].Utilization > 0 {
		vitalsRow2 = m.formatVitalsRow("Disk", m.data.DiskPercent, "GPU", m.data.GPUs[0].Utilization, boxWidth-4)
	} else {
		vitalsRow2 = m.formatVitalsRow("Disk", m.data.DiskPercent, "", 0, boxWidth-4)
	}
	sb.WriteString(m.boxLine(vitalsRow2, boxWidth))

	// Services section (if any)
	if len(m.data.Services) > 0 {
		sb.WriteString(m.divider(boxWidth))
		sb.WriteString(m.boxLine(lipgloss.NewStyle().Bold(true).Foreground(tui.ColorSecondary).Render("SERVICES"), boxWidth))

		// Header
		header := fmt.Sprintf("  %-18s %-14s %s", "NAME", "STATUS", "UPTIME")
		sb.WriteString(m.boxLine(tui.MutedStyle.Render(header), boxWidth))

		for _, svc := range m.data.Services {
			svcLine := m.formatServiceLine(svc, boxWidth-4)
			sb.WriteString(m.boxLine(svcLine, boxWidth))
		}
	}

	// Peers section (if any)
	if len(m.data.Peers) > 0 {
		sb.WriteString(m.divider(boxWidth))
		sb.WriteString(m.boxLine(lipgloss.NewStyle().Bold(true).Foreground(tui.ColorSecondary).Render("NETWORK PEERS"), boxWidth))

		for _, peer := range m.data.Peers {
			peerLine := m.formatPeerLine(peer)
			sb.WriteString(m.boxLine(peerLine, boxWidth))
		}
	}

	// Bottom border
	sb.WriteString(lipgloss.NewStyle().Foreground(tui.ColorBorder).Render("└"))
	sb.WriteString(lipgloss.NewStyle().Foreground(tui.ColorBorder).Render(strings.Repeat("─", boxWidth-2)))
	sb.WriteString(lipgloss.NewStyle().Foreground(tui.ColorBorder).Render("┘"))
	sb.WriteString("\n")

	// Help bar
	sb.WriteString(m.renderHelpBar())
	sb.WriteString("\n")

	// Status line
	sb.WriteString(m.renderStatusLine())

	return sb.String()
}

func (m StatusModel) boxLine(content string, width int) string {
	border := lipgloss.NewStyle().Foreground(tui.ColorBorder)
	// Calculate visible length (strip ANSI)
	visLen := visibleLength(content)
	padding := width - 4 - visLen
	if padding < 0 {
		padding = 0
	}
	return border.Render("│") + " " + content + strings.Repeat(" ", padding) + " " + border.Render("│") + "\n"
}

func (m StatusModel) divider(width int) string {
	border := lipgloss.NewStyle().Foreground(tui.ColorBorder)
	return border.Render("├") + border.Render(strings.Repeat("─", width-2)) + border.Render("┤") + "\n"
}

func (m StatusModel) formatNodeLine(width int) string {
	nodeName := m.data.NodeName
	if nodeName == "" {
		nodeName = "unknown"
	}
	nodeIP := m.data.NodeIP
	if nodeIP == "" {
		nodeIP = "-"
	}

	left := tui.LabelStyle.Render("Node: ") + nodeName
	right := tui.LabelStyle.Render("IP: ") + nodeIP

	leftLen := visibleLength(left)
	rightLen := visibleLength(right)
	spacing := width - leftLen - rightLen
	if spacing < 2 {
		spacing = 2
	}

	return left + strings.Repeat(" ", spacing) + right
}

func (m StatusModel) formatStatusLine(width int) string {
	org := m.data.OrgID
	if org == "" {
		org = "-"
	}

	var statusStr string
	if m.data.Connected {
		statusStr = tui.SuccessStyle.Render("🟢 ONLINE")
	} else {
		statusStr = tui.ErrorStyle.Render("🔴 OFFLINE")
	}

	left := tui.LabelStyle.Render("Organization: ") + org
	right := tui.LabelStyle.Render("Status: ") + statusStr

	leftLen := visibleLength(left)
	rightLen := visibleLength(right)
	spacing := width - leftLen - rightLen
	if spacing < 2 {
		spacing = 2
	}

	return left + strings.Repeat(" ", spacing) + right
}

func (m StatusModel) formatVitalsRow(label1 string, pct1 float64, label2 string, pct2 float64, width int) string {
	bar1 := m.progressBar(pct1, 10)
	item1 := fmt.Sprintf("  %s: %s %s", tui.LabelStyle.Render(label1), bar1, m.colorPercent(pct1))

	if label2 == "" {
		return item1
	}

	bar2 := m.progressBar(pct2, 10)
	item2 := fmt.Sprintf("%s: %s %s", tui.LabelStyle.Render(label2), bar2, m.colorPercent(pct2))

	item1Len := visibleLength(item1)
	item2Len := visibleLength(item2)
	spacing := width - item1Len - item2Len
	if spacing < 2 {
		spacing = 2
	}

	return item1 + strings.Repeat(" ", spacing) + item2
}

func (m StatusModel) progressBar(percent float64, width int) string {
	filled := int(percent / 100.0 * float64(width))
	if filled > width {
		filled = width
	}
	empty := width - filled

	var color lipgloss.Style
	switch {
	case percent >= 90:
		color = tui.ErrorStyle
	case percent >= 75:
		color = tui.WarningStyle
	default:
		color = tui.SuccessStyle
	}

	return color.Render(strings.Repeat("█", filled)) + tui.MutedStyle.Render(strings.Repeat("░", empty))
}

func (m StatusModel) colorPercent(pct float64) string {
	s := fmt.Sprintf("%5.1f%%", pct)
	switch {
	case pct >= 90:
		return tui.ErrorStyle.Render(s)
	case pct >= 75:
		return tui.WarningStyle.Render(s)
	default:
		return tui.SuccessStyle.Render(s)
	}
}

func (m StatusModel) formatServiceLine(svc ServiceStatus, width int) string {
	var statusIcon string
	switch svc.Status {
	case "running":
		statusIcon = tui.SuccessStyle.Render("🟢 running")
	case "stopped":
		statusIcon = tui.MutedStyle.Render("⚫ stopped")
	case "error":
		statusIcon = tui.ErrorStyle.Render("🔴 error")
	default:
		statusIcon = tui.WarningStyle.Render("🟡 " + svc.Status)
	}

	uptime := svc.Uptime
	if uptime == "" {
		uptime = "-"
	}

	return fmt.Sprintf("  %-18s %-14s %s", svc.Name, statusIcon, tui.MutedStyle.Render(uptime))
}

func (m StatusModel) formatPeerLine(peer PeerInfo) string {
	var indicator string
	if peer.Online {
		indicator = tui.SuccessStyle.Render("🟢")
	} else {
		indicator = tui.MutedStyle.Render("⚫")
	}

	name := peer.Hostname
	if peer.IP != "" {
		name += " " + tui.MutedStyle.Render(peer.IP)
	}

	extra := ""
	if peer.Online && peer.Latency != "" {
		extra = tui.MutedStyle.Render(peer.Latency)
		if peer.ConnType != "" {
			extra += " " + tui.MutedStyle.Render("["+peer.ConnType+"]")
		}
	}

	return fmt.Sprintf("  %s %s %s", indicator, name, extra)
}

func (m StatusModel) renderHelpBar() string {
	items := []string{
		lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary).Render("[r]") + tui.MutedStyle.Render("efresh"),
		lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary).Render("[a]") + tui.MutedStyle.Render("uto: "+boolStr(m.autoRefresh)),
		lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary).Render("[q]") + tui.MutedStyle.Render("uit"),
	}
	return "  " + strings.Join(items, "  ")
}

func (m StatusModel) renderStatusLine() string {
	if m.loading {
		return "  " + m.spinner.View() + " " + tui.SpinnerStyle.Render("Refreshing...")
	}
	if m.err != nil {
		return "  " + tui.ErrorStyle.Render("Error: "+m.err.Error())
	}

	lastUpdate := "never"
	if !m.refreshTimer.IsZero() {
		lastUpdate = m.refreshTimer.Format("15:04:05")
	}
	return "  " + tui.MutedStyle.Render("Last update: "+lastUpdate)
}

func boolStr(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// visibleLength returns the visible length of a string (excluding ANSI codes)
func visibleLength(s string) int {
	// Simple ANSI stripper
	inEscape := false
	length := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			inEscape = true
			i++
			continue
		}
		if inEscape {
			if (s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z') {
				inEscape = false
			}
			continue
		}
		length++
	}
	return length
}

// RunDashboard runs the interactive status dashboard
func RunDashboard(data StatusData, refreshFn func() (StatusData, error)) error {
	model := NewStatusModel(data, refreshFn)
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err := p.Run()
	return err
}
