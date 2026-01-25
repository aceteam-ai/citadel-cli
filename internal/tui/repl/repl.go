package repl

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/aceteam-ai/citadel-cli/internal/tui"
)

// Model is the BubbleTea model for the REPL
type Model struct {
	input       textinput.Model
	registry    *CommandRegistry
	completer   *Completer
	history     []string
	historyIdx  int
	suggestions []string
	selectedSug int
	output      string
	err         error
	quitting    bool
	version     string
	width       int
	height      int
	showWelcome bool
}

// Config holds REPL configuration
type Config struct {
	Version  string
	Services []string
}

// New creates a new REPL model
func New(cfg Config) Model {
	ti := textinput.New()
	ti.Placeholder = "type a command..."
	ti.Focus()
	ti.CharLimit = 256
	ti.Width = 50
	ti.PromptStyle = lipgloss.NewStyle().Foreground(tui.ColorPrimary).Bold(true)
	ti.Prompt = "❯ "

	registry := DefaultCommands()
	completer := NewCompleter(registry)
	completer.SetServices(cfg.Services)

	return Model{
		input:       ti,
		registry:    registry,
		completer:   completer,
		history:     []string{},
		historyIdx:  -1,
		version:     cfg.Version,
		showWelcome: true,
		selectedSug: -1,
	}
}

func (m Model) Init() tea.Cmd {
	return textinput.Blink
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			m.quitting = true
			return m, tea.Quit

		case tea.KeyEnter:
			// If suggestions visible and one is selected, use it
			if len(m.suggestions) > 0 && m.selectedSug >= 0 && m.selectedSug < len(m.suggestions) {
				m.input.SetValue(m.suggestions[m.selectedSug] + " ")
				m.input.CursorEnd()
				m.suggestions = nil
				m.selectedSug = -1
				return m, nil
			}

			input := strings.TrimSpace(m.input.Value())
			if input == "" {
				return m, nil
			}

			// Add to history
			m.history = append(m.history, input)
			m.historyIdx = len(m.history)

			// Clear
			m.input.SetValue("")
			m.suggestions = nil
			m.selectedSug = -1
			m.showWelcome = false

			// Execute command
			m.output, m.err = m.executeInput(input)

			if m.err == ErrQuit {
				m.quitting = true
				return m, tea.Quit
			}

			return m, nil

		case tea.KeyUp:
			if len(m.suggestions) > 0 {
				m.selectedSug--
				if m.selectedSug < 0 {
					m.selectedSug = len(m.suggestions) - 1
				}
				return m, nil
			}
			if len(m.history) > 0 && m.historyIdx > 0 {
				m.historyIdx--
				m.input.SetValue(m.history[m.historyIdx])
				m.input.CursorEnd()
			}
			return m, nil

		case tea.KeyDown:
			if len(m.suggestions) > 0 {
				m.selectedSug++
				if m.selectedSug >= len(m.suggestions) {
					m.selectedSug = 0
				}
				return m, nil
			}
			if m.historyIdx < len(m.history)-1 {
				m.historyIdx++
				m.input.SetValue(m.history[m.historyIdx])
				m.input.CursorEnd()
			} else if m.historyIdx == len(m.history)-1 {
				m.historyIdx = len(m.history)
				m.input.SetValue("")
			}
			return m, nil

		case tea.KeyTab:
			suggestions := m.completer.Complete(m.input.Value())
			if len(suggestions) == 1 {
				m.input.SetValue(suggestions[0] + " ")
				m.input.CursorEnd()
				m.suggestions = nil
				m.selectedSug = -1
			} else if len(suggestions) > 1 {
				m.suggestions = suggestions
				m.selectedSug = 0
				commonPrefix := FindLongestCommonPrefix(suggestions)
				if len(commonPrefix) > len(m.input.Value()) {
					m.input.SetValue(commonPrefix)
					m.input.CursorEnd()
				}
			}
			return m, nil

		case tea.KeyEsc:
			m.suggestions = nil
			m.selectedSug = -1
			return m, nil
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = min(msg.Width-10, 60)
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)

	// Update suggestions as user types
	if strings.HasPrefix(m.input.Value(), "/") {
		newSuggestions := m.completer.Complete(m.input.Value())
		if len(newSuggestions) > 0 && len(newSuggestions) <= 8 {
			m.suggestions = newSuggestions
			if m.selectedSug >= len(m.suggestions) {
				m.selectedSug = 0
			}
		} else {
			m.suggestions = nil
			m.selectedSug = -1
		}
	} else {
		m.suggestions = nil
		m.selectedSug = -1
	}

	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	var sb strings.Builder

	// Header
	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(tui.ColorPrimary).
		Render("⚡ Citadel")
	version := tui.MutedStyle.Render(" " + m.version)
	sb.WriteString(title + version + "\n")
	sb.WriteString(tui.MutedStyle.Render(strings.Repeat("─", 40)) + "\n\n")

	// Welcome or output
	if m.showWelcome {
		sb.WriteString(m.renderWelcome())
	} else if m.output != "" {
		// Compact output display
		lines := strings.Split(strings.TrimSpace(m.output), "\n")
		maxLines := 20
		if len(lines) > maxLines {
			lines = lines[:maxLines]
			lines = append(lines, tui.MutedStyle.Render("... (truncated)"))
		}
		for _, line := range lines {
			sb.WriteString("  " + line + "\n")
		}
		sb.WriteString("\n")
	}

	// Error
	if m.err != nil && m.err != ErrQuit {
		sb.WriteString(tui.ErrorStyle.Render("  ✗ "+m.err.Error()) + "\n\n")
	}

	// Input
	sb.WriteString(m.input.View() + "\n")

	// Suggestions (compact horizontal layout)
	if len(m.suggestions) > 0 {
		sb.WriteString("\n")
		var items []string
		for i, s := range m.suggestions {
			cmdName := strings.TrimPrefix(s, "/")
			if i == m.selectedSug {
				items = append(items, lipgloss.NewStyle().
					Bold(true).
					Foreground(tui.ColorPrimary).
					Background(lipgloss.Color("236")).
					Padding(0, 1).
					Render(s))
			} else {
				// Show description for unselected
				desc := ""
				if cmd := m.registry.Get(cmdName); cmd != nil {
					desc = " " + tui.MutedStyle.Render(cmd.Description)
				}
				items = append(items, tui.MutedStyle.Render("  "+s)+desc)
			}
		}
		sb.WriteString(strings.Join(items, "\n") + "\n")
	}

	// Footer hint
	sb.WriteString("\n" + tui.MutedStyle.Render("Tab: complete • ↑↓: navigate • Enter: run • Ctrl+C: quit"))

	return sb.String()
}

func (m Model) renderWelcome() string {
	var sb strings.Builder

	// Quick command reference
	commands := []struct {
		cmd  string
		desc string
	}{
		{"/status", "Show node status"},
		{"/services", "Manage services"},
		{"/logs", "View service logs"},
		{"/peers", "Network peers"},
		{"/help", "All commands"},
		{"/quit", "Exit"},
	}

	sb.WriteString("  " + tui.LabelStyle.Render("Commands:") + "\n")
	for _, c := range commands {
		cmd := lipgloss.NewStyle().Foreground(tui.ColorPrimary).Render(c.cmd)
		sb.WriteString(fmt.Sprintf("    %s %s\n", cmd, tui.MutedStyle.Render(c.desc)))
	}
	sb.WriteString("\n")

	return sb.String()
}

func (m *Model) executeInput(input string) (string, error) {
	if cmd, found := strings.CutPrefix(input, "/"); found {
		return m.executeCommand(cmd)
	}
	return "", fmt.Errorf("commands start with /  (try /help)")
}

func (m *Model) executeCommand(input string) (string, error) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return "", nil
	}

	cmdName := parts[0]
	args := parts[1:]

	cmd := m.registry.Get(cmdName)
	if cmd == nil {
		return "", fmt.Errorf("unknown: /%s (try /help)", cmdName)
	}

	switch cmd.Name {
	case "status":
		return m.runExternalCommand("citadel", "status")
	case "services":
		if len(args) == 0 {
			return m.runExternalCommand("citadel", "status")
		}
		if len(args) >= 2 {
			action := args[0]
			service := args[1]
			switch action {
			case "start":
				return m.runExternalCommand("citadel", "run", "--service", service)
			case "stop":
				return m.runExternalCommand("citadel", "stop", "--service", service)
			case "restart":
				m.runExternalCommand("citadel", "stop", "--service", service)
				return m.runExternalCommand("citadel", "run", "--service", service)
			}
		}
		return "", fmt.Errorf("usage: /services [start|stop|restart] <name>")
	case "logs":
		if len(args) == 0 {
			return "", fmt.Errorf("usage: /logs <service>")
		}
		return m.runExternalCommand("citadel", "logs", args[0])
	case "peers":
		return m.runExternalCommand("citadel", "status")
	case "jobs":
		return m.runExternalCommand("citadel", "status")
	case "version":
		return "Citadel " + m.version, nil
	}

	if cmd.Handler != nil {
		return "", cmd.Handler(args)
	}

	return "", fmt.Errorf("/%s not implemented", cmd.Name)
}

func (m *Model) runExternalCommand(name string, args ...string) (string, error) {
	execPath, err := os.Executable()
	if err != nil {
		execPath = name
	}
	cmd := exec.Command(execPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("failed: %v", err)
	}
	return string(output), nil
}

func Run(cfg Config) error {
	p := tea.NewProgram(New(cfg))
	_, err := p.Run()
	return err
}
