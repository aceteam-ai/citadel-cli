// internal/ui/devicecode.go
package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fatih/color"
	"github.com/mattn/go-runewidth"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// DeviceCodeModel represents the state for displaying device authorization code
type DeviceCodeModel struct {
	userCode        string
	verificationURI string
	expiresAt       time.Time
	status          string // "waiting", "approved", "error"
	errorMessage    string
	startTime       time.Time
	copyMessage     string    // temporary message when something is copied
	copyMessageTime time.Time // when the copy message was set
	showQR          bool      // whether to render the scannable enrollment QR
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
		showQR:          true, // QR is the primary enrollment affordance (issue #325)
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
	// Clear copy message after 2 seconds
	if m.copyMessage != "" && time.Since(m.copyMessageTime) > 2*time.Second {
		m.copyMessage = ""
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "Q":
			return m, tea.Quit
		case "c", "C":
			// Copy the complete URL to clipboard
			completeURL := m.verificationURI + "?code=" + m.userCode
			if err := platform.CopyToClipboard(completeURL); err != nil {
				m.copyMessage = "⚠️  Could not copy: " + err.Error()
			} else {
				m.copyMessage = "✓ Link copied!"
			}
			m.copyMessageTime = time.Now()
			return m, nil
		case "b", "B":
			// Open browser
			completeURL := m.verificationURI + "?code=" + m.userCode
			if err := platform.OpenURL(completeURL); err != nil {
				m.copyMessage = "⚠️  Could not open browser: " + err.Error()
			} else {
				m.copyMessage = "✓ Opening browser..."
			}
			m.copyMessageTime = time.Now()
			return m, nil
		case "s", "S":
			// Toggle the scannable enrollment QR code
			m.showQR = !m.showQR
			return m, nil
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
	const boxWidth = 60

	// Helper function to create a padded line
	padLine := func(content string, leftPad int) string {
		visibleLen := runewidth.StringWidth(stripANSI(content))
		rightPad := boxWidth - leftPad - visibleLen
		if rightPad < 0 {
			rightPad = 0
		}
		return "│" + strings.Repeat(" ", leftPad) + content + strings.Repeat(" ", rightPad) + "│\n"
	}

	// Helper to create a padded line with a clickable hyperlink
	padLineHyperlink := func(displayText, plainText string, leftPad int) string {
		visibleLen := runewidth.StringWidth(plainText)
		rightPad := boxWidth - leftPad - visibleLen
		if rightPad < 0 {
			rightPad = 0
		}
		return "│" + strings.Repeat(" ", leftPad) + displayText + strings.Repeat(" ", rightPad) + "│\n"
	}

	completeURL := m.verificationURI + "?code=" + m.userCode

	// Scannable enrollment QR (rendered above the box; QR + quiet zone is
	// wider than the box). Scanning binds this node to your org's Fabric.
	if m.showQR && m.status == "waiting" {
		sb.WriteString("\n")
		sb.WriteString(centerText("📱 Scan to add this node to your Fabric", boxWidth+2) + "\n")
		sb.WriteString(RenderEnrollQR(m.verificationURI, m.userCode))
		sb.WriteString("\n")
	}

	// Top border
	sb.WriteString("┌" + strings.Repeat("─", boxWidth) + "┐\n")
	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")
	sb.WriteString("│" + centerText("🔐 Device Authorization", boxWidth) + "│\n")
	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")

	// Simple instruction + clickable URL
	sb.WriteString(padLine("Open this link to sign in:", 2))
	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")

	// Show clickable complete URL (with code baked in)
	clickableURL := Hyperlink(completeURL, completeURL)
	// Truncate display if too long, but keep full URL in hyperlink
	displayURL := completeURL
	if len(displayURL) > boxWidth-6 {
		displayURL = displayURL[:boxWidth-9] + "..."
	}
	clickableURL = Hyperlink(completeURL, displayURL)
	sb.WriteString(padLineHyperlink(clickableURL, displayURL, 3))
	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")

	// Code box (for manual entry if needed)
	sb.WriteString(padLine("Or enter this code manually:", 2))
	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")
	codeBox := "╔══════════════╗"
	sb.WriteString("│" + centerText(codeBox, boxWidth) + "│\n")
	plainCodeText := fmt.Sprintf("║  %s   ║", m.userCode)
	coloredCodeText := fmt.Sprintf("║  %s   ║", color.CyanString(m.userCode))
	sb.WriteString("│" + centerTextColored(coloredCodeText, plainCodeText, boxWidth) + "│\n")
	sb.WriteString("│" + centerText("╚══════════════╝", boxWidth) + "│\n")
	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")

	// Hotkeys - prominent, inside a visual separator
	if m.status == "waiting" {
		sb.WriteString("│" + strings.Repeat("─", boxWidth) + "│\n")
		qrLabel := "[S] Show QR"
		if m.showQR {
			qrLabel = "[S] Hide QR"
		}
		sb.WriteString(padLine("KEYBOARD SHORTCUTS:", 2))
		sb.WriteString(padLine("  [B] Browser   [C] Copy   "+qrLabel+"   [Q] Quit", 2))
		sb.WriteString("│" + strings.Repeat("─", boxWidth) + "│\n")
	}

	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")

	// Status
	if m.status == "waiting" {
		remaining := time.Until(m.expiresAt)
		if remaining < 0 {
			remaining = 0
		}
		minutes := int(remaining.Minutes())
		seconds := int(remaining.Seconds()) % 60

		statusText := fmt.Sprintf("⏳ Waiting... (%d:%02d)", minutes, seconds)
		sb.WriteString(padLine(statusText, 2))
	} else if m.status == "approved" {
		plainText := "✅ Authorization successful!"
		coloredText := color.GreenString(plainText)
		sb.WriteString(padLineColored(coloredText, plainText, 2))
	} else if m.status == "error" {
		plainText := "❌ " + m.errorMessage
		coloredText := color.RedString(plainText)
		sb.WriteString(padLineColored(coloredText, plainText, 2))
	}

	// Copy message (shown temporarily after copying)
	if m.copyMessage != "" {
		sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")
		plainMsg := m.copyMessage
		coloredMsg := color.GreenString(m.copyMessage)
		if strings.HasPrefix(m.copyMessage, "⚠️") {
			coloredMsg = color.YellowString(m.copyMessage)
		}
		sb.WriteString(padLineColored(coloredMsg, plainMsg, 2))
	}

	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")

	// Bottom border
	sb.WriteString("└" + strings.Repeat("─", boxWidth) + "┘\n")

	return sb.String()
}

// padLineColored creates a padded line with colored text
func padLineColored(coloredContent, plainContent string, leftPad int) string {
	const boxWidth = 63
	visibleLen := runewidth.StringWidth(plainContent)
	rightPad := boxWidth - leftPad - visibleLen
	if rightPad < 0 {
		rightPad = 0
	}
	return "│" + strings.Repeat(" ", leftPad) + coloredContent + strings.Repeat(" ", rightPad) + "│\n"
}

// centerText centers text within a given width
func centerText(text string, width int) string {
	textLen := runewidth.StringWidth(text)
	if textLen >= width {
		return text
	}
	leftPad := (width - textLen) / 2
	rightPad := width - textLen - leftPad
	return strings.Repeat(" ", leftPad) + text + strings.Repeat(" ", rightPad)
}

// centerTextColored centers colored text within a given width
func centerTextColored(coloredText, plainText string, width int) string {
	textLen := runewidth.StringWidth(plainText)
	if textLen >= width {
		return coloredText
	}
	leftPad := (width - textLen) / 2
	rightPad := width - textLen - leftPad
	return strings.Repeat(" ", leftPad) + coloredText + strings.Repeat(" ", rightPad)
}

// stripANSI removes ANSI color codes from a string
func stripANSI(str string) string {
	// Simple ANSI stripper - removes escape sequences
	var result strings.Builder
	inEscape := false
	for i := 0; i < len(str); i++ {
		if str[i] == '\x1b' && i+1 < len(str) && str[i+1] == '[' {
			inEscape = true
			i++ // skip '['
			continue
		}
		if inEscape {
			if (str[i] >= 'A' && str[i] <= 'Z') || (str[i] >= 'a' && str[i] <= 'z') {
				inEscape = false
			}
			continue
		}
		result.WriteByte(str[i])
	}
	return result.String()
}

// NewDeviceCodeProgram creates a new tea.Program for device code display
// This allows external callers to send status updates via UpdateStatus
func NewDeviceCodeProgram(model DeviceCodeModel) *tea.Program {
	return tea.NewProgram(model)
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
		return fmt.Errorf("%s", m.errorMessage)
	}

	return nil
}

// UpdateStatus sends a status update to a running program
// Use this from polling goroutine to update the UI
func UpdateStatus(p *tea.Program, status string) {
	p.Send(status)
}
