// Package dashboard provides the interactive status dashboard for Citadel CLI.
package dashboard

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/aceteam-ai/citadel-cli/internal/tui"
)

// Panel represents a bordered panel in the dashboard
type Panel struct {
	Title   string
	Content string
	Width   int
	Active  bool
}

// Render renders the panel with a border and title
func (p Panel) Render() string {
	style := tui.PanelStyle
	if p.Active {
		style = tui.ActivePanelStyle
	}

	if p.Width > 0 {
		style = style.Width(p.Width)
	}

	titleStyle := tui.SubtitleStyle
	if p.Active {
		titleStyle = titleStyle.Foreground(tui.ColorPrimary)
	}

	var sb strings.Builder
	if p.Title != "" {
		sb.WriteString(titleStyle.Render(p.Title))
		sb.WriteString("\n")
	}
	sb.WriteString(p.Content)

	return style.Render(sb.String())
}

// KeyValuePanel creates a panel with key-value pairs
type KeyValuePanel struct {
	Title  string
	Items  []KeyValue
	Width  int
	Active bool
}

// KeyValue represents a key-value pair
type KeyValue struct {
	Key   string
	Value string
	Style lipgloss.Style // Optional style for the value
}

// Render renders the key-value panel
func (p KeyValuePanel) Render() string {
	var lines []string
	maxKeyLen := 0

	for _, item := range p.Items {
		if len(item.Key) > maxKeyLen {
			maxKeyLen = len(item.Key)
		}
	}

	for _, item := range p.Items {
		key := tui.LabelStyle.Render(padRight(item.Key+":", maxKeyLen+1))
		value := item.Value
		if item.Style.Value() != "" {
			value = item.Style.Render(value)
		} else {
			value = tui.ValueStyle.Render(value)
		}
		lines = append(lines, key+" "+value)
	}

	panel := Panel{
		Title:   p.Title,
		Content: strings.Join(lines, "\n"),
		Width:   p.Width,
		Active:  p.Active,
	}
	return panel.Render()
}

// ProgressPanel creates a panel with progress bars
type ProgressPanel struct {
	Title  string
	Items  []ProgressItem
	Width  int
	Active bool
}

// ProgressItem represents a metric with a progress bar
type ProgressItem struct {
	Label   string
	Percent float64
	Detail  string // e.g., "4.2 GB / 16 GB"
}

// Render renders the progress panel
func (p ProgressPanel) Render() string {
	var lines []string
	barWidth := 20 // Default bar width

	for _, item := range p.Items {
		// Label with fixed width
		label := tui.LabelStyle.Render(padRight(item.Label+":", 12))

		// Progress bar
		bar := tui.ProgressBar(item.Percent, barWidth)

		// Percentage
		pctStyle := tui.SuccessStyle
		if item.Percent >= 90 {
			pctStyle = tui.ErrorStyle
		} else if item.Percent >= 75 {
			pctStyle = tui.WarningStyle
		}
		pct := pctStyle.Render(fmt.Sprintf("%5.1f%%", item.Percent))

		// Detail (optional)
		detail := ""
		if item.Detail != "" {
			detail = " " + tui.MutedStyle.Render("("+item.Detail+")")
		}

		lines = append(lines, label+" "+bar+" "+pct+detail)
	}

	panel := Panel{
		Title:   p.Title,
		Content: strings.Join(lines, "\n"),
		Width:   p.Width,
		Active:  p.Active,
	}
	return panel.Render()
}

// ServicePanel creates a panel showing service status
type ServicePanel struct {
	Title    string
	Services []ServiceStatus
	Width    int
	Active   bool
}

// ServiceStatus represents a service's status
type ServiceStatus struct {
	Name   string
	Status string // "running", "stopped", "error"
	Uptime string // e.g., "2d 14h"
}

// Render renders the service panel
func (p ServicePanel) Render() string {
	var lines []string

	// Header
	header := tui.MutedStyle.Render(fmt.Sprintf("%-16s %-12s %s", "SERVICE", "STATUS", "UPTIME"))
	lines = append(lines, header)

	for _, svc := range p.Services {
		// Status indicator and text
		var statusStr string
		switch svc.Status {
		case "running":
			statusStr = tui.SuccessStyle.Render("● running")
		case "stopped":
			statusStr = tui.MutedStyle.Render("○ stopped")
		case "error":
			statusStr = tui.ErrorStyle.Render("✗ error")
		default:
			statusStr = tui.WarningStyle.Render("? " + svc.Status)
		}

		uptime := svc.Uptime
		if uptime == "" {
			uptime = "-"
		}

		line := fmt.Sprintf("  %-14s %-12s %s", svc.Name, statusStr, tui.MutedStyle.Render(uptime))
		lines = append(lines, line)
	}

	panel := Panel{
		Title:   p.Title,
		Content: strings.Join(lines, "\n"),
		Width:   p.Width,
		Active:  p.Active,
	}
	return panel.Render()
}

// PeerPanel creates a panel showing network peers
type PeerPanel struct {
	Title  string
	Peers  []PeerInfo
	Width  int
	Active bool
}

// PeerInfo represents a network peer
type PeerInfo struct {
	Hostname  string
	IP        string
	Online    bool
	Latency   string // e.g., "12ms"
	ConnType  string // "direct" or "relay"
}

// Render renders the peer panel
func (p PeerPanel) Render() string {
	if len(p.Peers) == 0 {
		panel := Panel{
			Title:   p.Title,
			Content: tui.MutedStyle.Render("No peers connected"),
			Width:   p.Width,
			Active:  p.Active,
		}
		return panel.Render()
	}

	var lines []string
	for _, peer := range p.Peers {
		indicator := tui.StatusIndicator(peer.Online)
		name := peer.Hostname
		if peer.IP != "" {
			name += " " + tui.MutedStyle.Render(peer.IP)
		}

		extra := ""
		if peer.Online {
			if peer.Latency != "" {
				extra = tui.MutedStyle.Render(peer.Latency)
			}
			if peer.ConnType != "" {
				if extra != "" {
					extra += " "
				}
				extra += tui.MutedStyle.Render("[" + peer.ConnType + "]")
			}
		}

		line := fmt.Sprintf("  %s %s %s", indicator, name, extra)
		lines = append(lines, line)
	}

	panel := Panel{
		Title:   p.Title,
		Content: strings.Join(lines, "\n"),
		Width:   p.Width,
		Active:  p.Active,
	}
	return panel.Render()
}

// HelpBar renders a help bar at the bottom of the screen
func HelpBar(items []HelpItem) string {
	var parts []string
	for _, item := range items {
		key := lipgloss.NewStyle().
			Bold(true).
			Foreground(tui.ColorPrimary).
			Render("[" + item.Key + "]")
		label := tui.MutedStyle.Render(item.Label)
		parts = append(parts, key+label)
	}
	return strings.Join(parts, "  ")
}

// HelpItem represents a keyboard shortcut
type HelpItem struct {
	Key   string
	Label string
}

// Helper functions

func padRight(s string, length int) string {
	if len(s) >= length {
		return s
	}
	return s + strings.Repeat(" ", length-len(s))
}
