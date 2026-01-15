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
	// Clear copy message after 2 seconds
	if m.copyMessage != "" && time.Since(m.copyMessageTime) > 2*time.Second {
		m.copyMessage = ""
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "c":
			// Copy the complete URL to clipboard
			completeURL := m.verificationURI + "?code=" + m.userCode
			if err := platform.CopyToClipboard(completeURL); err != nil {
				m.copyMessage = "⚠️  Could not copy: " + err.Error()
			} else {
				m.copyMessage = "✓ URL copied to clipboard!"
			}
			m.copyMessageTime = time.Now()
			return m, nil
		case "C":
			// Copy just the code to clipboard
			if err := platform.CopyToClipboard(m.userCode); err != nil {
				m.copyMessage = "⚠️  Could not copy: " + err.Error()
			} else {
				m.copyMessage = "✓ Code copied to clipboard!"
			}
			m.copyMessageTime = time.Now()
			return m, nil
		case "b", "B":
			// Open browser manually
			completeURL := m.verificationURI + "?code=" + m.userCode
			if err := platform.OpenURL(completeURL); err != nil {
				m.copyMessage = "⚠️  Could not open browser: " + err.Error()
			} else {
				m.copyMessage = "✓ Browser opened!"
			}
			m.copyMessageTime = time.Now()
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
	const boxWidth = 63

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
	// plainText is for width calculation, displayText contains the hyperlink
	padLineHyperlink := func(displayText, plainText string, leftPad int) string {
		visibleLen := runewidth.StringWidth(plainText)
		rightPad := boxWidth - leftPad - visibleLen
		if rightPad < 0 {
			rightPad = 0
		}
		return "│" + strings.Repeat(" ", leftPad) + displayText + strings.Repeat(" ", rightPad) + "│\n"
	}

	// Top border
	sb.WriteString("┌" + strings.Repeat("─", boxWidth) + "┐\n")
	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")
	sb.WriteString("│" + centerText("Device Authorization", boxWidth) + "│\n")
	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")
	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")

	// Instructions
	sb.WriteString(padLine("To complete setup, visit this URL in your browser:", 2))
	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")

	// Make the URL clickable using OSC 8 hyperlink
	completeURL := m.verificationURI + "?code=" + m.userCode
	clickableURL := Hyperlink(completeURL, m.verificationURI)
	sb.WriteString(padLineHyperlink(clickableURL, m.verificationURI, 4))
	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")
	sb.WriteString(padLine("and enter the following code:", 2))
	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")

	// Code box (emphasized)
	codeBox := "╔══════════════╗"
	sb.WriteString("│" + centerText(codeBox, boxWidth) + "│\n")

	plainCodeText := fmt.Sprintf("║  %s   ║", m.userCode)
	coloredCodeText := fmt.Sprintf("║  %s   ║", color.CyanString(m.userCode))
	sb.WriteString("│" + centerTextColored(coloredCodeText, plainCodeText, boxWidth) + "│\n")

	sb.WriteString("│" + centerText("╚══════════════╝", boxWidth) + "│\n")
	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")

	// Status
	if m.status == "waiting" {
		remaining := time.Until(m.expiresAt)
		if remaining < 0 {
			remaining = 0
		}
		minutes := int(remaining.Minutes())
		seconds := int(remaining.Seconds()) % 60

		statusText := fmt.Sprintf("⏳ Waiting for authorization... (%d:%02d remaining)", minutes, seconds)
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

	sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")

	// Copy message (shown temporarily after copying)
	if m.copyMessage != "" {
		plainMsg := m.copyMessage
		coloredMsg := color.GreenString(m.copyMessage)
		if strings.HasPrefix(m.copyMessage, "⚠️") {
			coloredMsg = color.YellowString(m.copyMessage)
		}
		sb.WriteString(padLineColored(coloredMsg, plainMsg, 2))
		sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")
	}

	// Keyboard shortcuts and browser hint
	if m.status == "waiting" {
		dimStyle := color.New(color.Faint)
		shortcutsPlain := "'b' browser  •  'c' copy URL  •  'C' copy code"
		shortcutsStyled := dimStyle.Sprint(shortcutsPlain)
		sb.WriteString(padLineColored(shortcutsStyled, shortcutsPlain, 2))

		// Show complete URL as clickable link
		if len(completeURL) <= 57 {
			clickableComplete := Hyperlink(completeURL, completeURL)
			sb.WriteString(padLineHyperlink(clickableComplete, completeURL, 2))
		}
		sb.WriteString("│" + strings.Repeat(" ", boxWidth) + "│\n")
	}

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
