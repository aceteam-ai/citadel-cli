// Package tui provides rich terminal user interface components for Citadel CLI.
package tui

import (
	"os"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// Color palette - AceTeam brand colors
var (
	ColorPrimary   = lipgloss.AdaptiveColor{Light: "#5A67D8", Dark: "#7C3AED"}
	ColorSecondary = lipgloss.AdaptiveColor{Light: "#38B2AC", Dark: "#4FD1C5"}
	ColorSuccess   = lipgloss.AdaptiveColor{Light: "#38A169", Dark: "#48BB78"}
	ColorWarning   = lipgloss.AdaptiveColor{Light: "#D69E2E", Dark: "#F6E05E"}
	ColorError     = lipgloss.AdaptiveColor{Light: "#E53E3E", Dark: "#FC8181"}
	ColorMuted     = lipgloss.AdaptiveColor{Light: "#718096", Dark: "#A0AEC0"}
	ColorText      = lipgloss.AdaptiveColor{Light: "#1A202C", Dark: "#F7FAFC"}
	ColorBorder    = lipgloss.AdaptiveColor{Light: "#CBD5E0", Dark: "#4A5568"}
)

// Base styles
var (
	// Title style for headers
	TitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorPrimary).
			MarginBottom(1)

	// SubtitleStyle for section headers
	SubtitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorSecondary)

	// LabelStyle for key names in key-value pairs
	LabelStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorMuted)

	// ValueStyle for values
	ValueStyle = lipgloss.NewStyle().
			Foreground(ColorText)

	// SuccessStyle for success messages
	SuccessStyle = lipgloss.NewStyle().
			Foreground(ColorSuccess)

	// WarningStyle for warning messages
	WarningStyle = lipgloss.NewStyle().
			Foreground(ColorWarning)

	// ErrorStyle for error messages
	ErrorStyle = lipgloss.NewStyle().
			Foreground(ColorError)

	// MutedStyle for less important text
	MutedStyle = lipgloss.NewStyle().
			Foreground(ColorMuted)

	// SpinnerStyle for spinner text
	SpinnerStyle = lipgloss.NewStyle().
			Foreground(ColorPrimary)

	// PromptStyle for the REPL prompt
	PromptStyle = lipgloss.NewStyle().
			Foreground(ColorPrimary).
			Bold(true)
)

// Panel styles for dashboard
var (
	// PanelStyle for bordered panels
	PanelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorBorder).
			Padding(1, 2)

	// ActivePanelStyle for selected/active panels
	ActivePanelStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(ColorPrimary).
				Padding(1, 2)

	// HeaderPanelStyle for panel headers
	HeaderPanelStyle = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder(), false, false, true, false).
				BorderForeground(ColorBorder).
				MarginBottom(1)
)

// Progress bar styles
var (
	// ProgressBarFilled style for filled portion
	ProgressBarFilled = lipgloss.NewStyle().
				Foreground(ColorSuccess)

	// ProgressBarEmpty style for empty portion
	ProgressBarEmpty = lipgloss.NewStyle().
				Foreground(ColorMuted)

	// ProgressBarWarning style when usage is high
	ProgressBarWarning = lipgloss.NewStyle().
				Foreground(ColorWarning)

	// ProgressBarCritical style when usage is critical
	ProgressBarCritical = lipgloss.NewStyle().
				Foreground(ColorError)
)

// Status indicator styles
var (
	StatusOnline  = SuccessStyle.Render("●")
	StatusOffline = ErrorStyle.Render("●")
	StatusWarning = WarningStyle.Render("●")
	StatusUnknown = MutedStyle.Render("●")
)

// IsTTY returns true if stdout is a terminal
func IsTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// ShouldUseInteractive determines if interactive TUI should be used
func ShouldUseInteractive(forceInteractive, noColor bool) bool {
	// Disable for non-TTY (pipes, scripts)
	if !IsTTY() {
		return false
	}
	// Respect --no-color flag
	if noColor {
		return false
	}
	// Force interactive if requested
	if forceInteractive {
		return true
	}
	return true
}

// ProgressBar renders a progress bar with appropriate colors based on usage
func ProgressBar(percent float64, width int) string {
	if width <= 0 {
		width = 20
	}

	filled := min(int(percent/100.0*float64(width)), width)
	empty := width - filled

	// Choose color based on percentage
	var style lipgloss.Style
	switch {
	case percent >= 90:
		style = ProgressBarCritical
	case percent >= 75:
		style = ProgressBarWarning
	default:
		style = ProgressBarFilled
	}

	bar := style.Render(repeat("█", filled)) + ProgressBarEmpty.Render(repeat("░", empty))
	return bar
}

// Helper function to repeat a string
func repeat(s string, n int) string {
	if n <= 0 {
		return ""
	}
	result := ""
	for range n {
		result += s
	}
	return result
}

// StatusIndicator returns a colored status indicator
func StatusIndicator(online bool) string {
	if online {
		return StatusOnline
	}
	return StatusOffline
}

// FormatKeyValue formats a key-value pair
func FormatKeyValue(key, value string) string {
	return LabelStyle.Render(key+":") + " " + ValueStyle.Render(value)
}
