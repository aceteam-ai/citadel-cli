// internal/ui/devicecode.go
package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fatih/color"
)

// DeviceCodeModel represents the state for displaying device authorization code
type DeviceCodeModel struct {
	userCode        string
	verificationURI string
	expiresAt       time.Time
	status          string // "waiting", "approved", "error"
	errorMessage    string
	startTime       time.Time
}

type tickMsg time.Time

// NewDeviceCodeModel creates a new device code display model
func NewDeviceCodeModel(userCode, verificationURI string, expiresIn int) DeviceCodeModel {
	now := time.Now()
	return DeviceCodeModel{
		userCode:        userCode,
		verificationURI: verificationURI,
		expiresAt:       now.Add(time.Duration(expiresIn) * time.Second),
		status:          "waiting",
		startTime:       now,
	}
}

func (m DeviceCodeModel) Init() tea.Cmd {
	return tickCmd()
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m DeviceCodeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		}
	case tickMsg:
		// Update every second to refresh countdown
		return m, tickCmd()
	case string:
		// Status update from external caller
		if msg == "approved" {
			m.status = "approved"
			return m, tea.Quit
		} else if strings.HasPrefix(msg, "error:") {
			m.status = "error"
			m.errorMessage = strings.TrimPrefix(msg, "error:")
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m DeviceCodeModel) View() string {
	var sb strings.Builder

	// Top border
	sb.WriteString("┌" + strings.Repeat("─", 63) + "┐\n")
	sb.WriteString("│" + centerText("", 63) + "│\n")
	sb.WriteString("│" + centerText("Authenticate with AceTeam Nexus", 63) + "│\n")
	sb.WriteString("│" + centerText("", 63) + "│\n")
	sb.WriteString("│" + strings.Repeat(" ", 63) + "│\n")

	// Instructions
	sb.WriteString("│   To complete setup, visit this URL in your browser:        │\n")
	sb.WriteString("│" + strings.Repeat(" ", 63) + "│\n")
	sb.WriteString("│     " + m.verificationURI + strings.Repeat(" ", 63-6-len(m.verificationURI)) + "│\n")
	sb.WriteString("│" + strings.Repeat(" ", 63) + "│\n")
	sb.WriteString("│   and enter the following code:                              │\n")
	sb.WriteString("│" + strings.Repeat(" ", 63) + "│\n")

	// Code box (emphasized)
	codeBox := "╔══════════════╗"
	sb.WriteString("│" + centerText(codeBox, 63) + "│\n")
	codeText := fmt.Sprintf("║  %s   ║", m.userCode)
	sb.WriteString("│" + centerText(color.CyanString(codeText), 63+len(color.CyanString(""))-len(codeText)) + "│\n")
	sb.WriteString("│" + centerText("╚══════════════╝", 63) + "│\n")
	sb.WriteString("│" + strings.Repeat(" ", 63) + "│\n")

	// Status
	if m.status == "waiting" {
		remaining := time.Until(m.expiresAt)
		if remaining < 0 {
			remaining = 0
		}
		minutes := int(remaining.Minutes())
		seconds := int(remaining.Seconds()) % 60

		statusText := fmt.Sprintf("⏳ Waiting for authorization... (%d:%02d remaining)", minutes, seconds)
		sb.WriteString("│   " + statusText + strings.Repeat(" ", 63-3-len(statusText)) + "│\n")
	} else if m.status == "approved" {
		statusText := color.GreenString("✅ Authorization successful!")
		plainText := "✅ Authorization successful!"
		sb.WriteString("│   " + statusText + strings.Repeat(" ", 63-3-len(plainText)) + "│\n")
	} else if m.status == "error" {
		statusText := color.RedString("❌ " + m.errorMessage)
		plainText := "❌ " + m.errorMessage
		sb.WriteString("│   " + statusText + strings.Repeat(" ", 63-3-len(plainText)) + "│\n")
	}

	sb.WriteString("│" + strings.Repeat(" ", 63) + "│\n")

	// Browser hint
	if m.status == "waiting" {
		sb.WriteString("│   Browser didn't open? Copy the URL above or visit:          │\n")
		completeURI := m.verificationURI + "?code=" + m.userCode
		if len(completeURI) <= 55 {
			sb.WriteString("│   " + completeURI + strings.Repeat(" ", 63-3-len(completeURI)) + "│\n")
		} else {
			sb.WriteString("│   " + m.verificationURI + strings.Repeat(" ", 63-3-len(m.verificationURI)) + "│\n")
		}
		sb.WriteString("│" + strings.Repeat(" ", 63) + "│\n")
	}

	// Bottom border
	sb.WriteString("└" + strings.Repeat("─", 63) + "┘\n")

	return sb.String()
}

// centerText centers text within a given width
func centerText(text string, width int) string {
	textLen := len(text)
	if textLen >= width {
		return text
	}
	leftPad := (width - textLen) / 2
	rightPad := width - textLen - leftPad
	return strings.Repeat(" ", leftPad) + text + strings.Repeat(" ", rightPad)
}

// DisplayDeviceCode shows the device code and waits for authorization
// This is a helper function that can be called from command code
func DisplayDeviceCode(userCode, verificationURI string, expiresIn int) error {
	model := NewDeviceCodeModel(userCode, verificationURI, expiresIn)
	p := tea.NewProgram(model)

	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("failed to display device code: %w", err)
	}

	// Check final status
	m := finalModel.(DeviceCodeModel)
	if m.status == "error" {
		return fmt.Errorf(m.errorMessage)
	}

	return nil
}

// UpdateStatus sends a status update to a running program
// Use this from polling goroutine to update the UI
func UpdateStatus(p *tea.Program, status string) {
	p.Send(status)
}
