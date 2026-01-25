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
	input          textinput.Model
	registry       *CommandRegistry
	completer      *Completer
	history        []string
	historyIdx     int
	suggestions    []string
	selectedSug    int  // Currently selected suggestion index
	showSuggestions bool
	output         string
	err            error
	quitting       bool
	version        string
	width          int
	height         int
}

// Config holds REPL configuration
type Config struct {
	Version  string
	Services []string
}

// New creates a new REPL model
func New(cfg Config) Model {
	ti := textinput.New()
	ti.Placeholder = "Type /help for commands"
	ti.Focus()
	ti.CharLimit = 256
	ti.Width = 60
	ti.PromptStyle = lipgloss.NewStyle().Foreground(tui.ColorPrimary).Bold(true)
	ti.Prompt = "citadel> "

	registry := DefaultCommands()
	completer := NewCompleter(registry)
	completer.SetServices(cfg.Services)

	return Model{
		input:      ti,
		registry:   registry,
		completer:  completer,
		history:    []string{},
		historyIdx: -1,
		version:    cfg.Version,
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
			if m.showSuggestions && len(m.suggestions) > 0 && m.selectedSug >= 0 {
				m.input.SetValue(m.suggestions[m.selectedSug] + " ")
				m.input.CursorEnd()
				m.showSuggestions = false
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

			// Clear input and suggestions
			m.input.SetValue("")
			m.suggestions = nil
			m.showSuggestions = false
			m.selectedSug = -1

			// Execute command
			m.output, m.err = m.executeInput(input)

			// Check for quit
			if m.err == ErrQuit {
				m.quitting = true
				return m, tea.Quit
			}

			return m, nil

		case tea.KeyUp:
			// Navigate suggestions if visible
			if m.showSuggestions && len(m.suggestions) > 0 {
				m.selectedSug--
				if m.selectedSug < 0 {
					m.selectedSug = len(m.suggestions) - 1
				}
				return m, nil
			}
			// Navigate history
			if len(m.history) > 0 && m.historyIdx > 0 {
				m.historyIdx--
				m.input.SetValue(m.history[m.historyIdx])
				m.input.CursorEnd()
			}
			return m, nil

		case tea.KeyDown:
			// Navigate suggestions if visible
			if m.showSuggestions && len(m.suggestions) > 0 {
				m.selectedSug++
				if m.selectedSug >= len(m.suggestions) {
					m.selectedSug = 0
				}
				return m, nil
			}
			// Navigate history
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
			// Tab completion
			suggestions := m.completer.Complete(m.input.Value())
			if len(suggestions) == 1 {
				// Single match - complete it
				m.input.SetValue(suggestions[0] + " ")
				m.input.CursorEnd()
				m.showSuggestions = false
				m.selectedSug = -1
			} else if len(suggestions) > 1 {
				// Multiple matches - show suggestions and complete common prefix
				m.suggestions = suggestions
				m.showSuggestions = true
				m.selectedSug = 0
				commonPrefix := FindLongestCommonPrefix(suggestions)
				if len(commonPrefix) > len(m.input.Value()) {
					m.input.SetValue(commonPrefix)
					m.input.CursorEnd()
				}
			}
			return m, nil

		case tea.KeyEsc:
			// Hide suggestions
			m.showSuggestions = false
			m.selectedSug = -1
			return m, nil
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = msg.Width - 12
		return m, nil
	}

	// Update text input
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)

	// Update suggestions as user types (but keep them visible)
	if strings.HasPrefix(m.input.Value(), "/") {
		newSuggestions := m.completer.Complete(m.input.Value())
		if len(newSuggestions) > 0 && len(newSuggestions) <= 12 {
			m.suggestions = newSuggestions
			m.showSuggestions = true
			// Reset selection if suggestions changed
			if m.selectedSug >= len(m.suggestions) {
				m.selectedSug = 0
			}
		} else if len(newSuggestions) == 0 {
			m.showSuggestions = false
			m.selectedSug = -1
		}
	} else if m.input.Value() == "" {
		// Keep suggestions visible even when input is cleared to show available commands
		m.suggestions = m.completer.Complete("/")
		if len(m.suggestions) > 0 {
			m.showSuggestions = true
			m.selectedSug = -1
		}
	}

	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	var sb strings.Builder

	// Header box
	headerStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tui.ColorBorder).
		Padding(0, 2).
		Width(50)

	header := lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary).Render("⚡ Citadel Interactive Mode")
	header += " " + tui.MutedStyle.Render("("+m.version+")")
	sb.WriteString(headerStyle.Render(header))
	sb.WriteString("\n\n")

	// Output from last command
	if m.output != "" {
		outputStyle := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(tui.ColorMuted).
			Padding(0, 1).
			MaxWidth(m.width - 4)
		sb.WriteString(outputStyle.Render(strings.TrimSpace(m.output)))
		sb.WriteString("\n\n")
	}

	// Error from last command
	if m.err != nil && m.err != ErrQuit {
		errorStyle := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(tui.ColorError).
			Padding(0, 1)
		sb.WriteString(errorStyle.Render(tui.ErrorStyle.Render("✗ " + m.err.Error())))
		sb.WriteString("\n\n")
	}

	// Input line
	sb.WriteString(m.input.View())
	sb.WriteString("\n")

	// Suggestions panel (always below input when visible)
	if m.showSuggestions && len(m.suggestions) > 0 {
		sb.WriteString("\n")

		sugStyle := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(tui.ColorSecondary).
			Padding(0, 1)

		var sugLines []string
		sugLines = append(sugLines, tui.MutedStyle.Render("Tab completions (↑/↓ to select, Enter to use):"))
		sugLines = append(sugLines, "")

		for i, s := range m.suggestions {
			prefix := "  "
			style := tui.ValueStyle
			if i == m.selectedSug {
				prefix = tui.SuccessStyle.Render("▸ ")
				style = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)
			}
			// Get command description if available
			cmdName := strings.TrimPrefix(s, "/")
			desc := ""
			if cmd := m.registry.Get(cmdName); cmd != nil {
				desc = " " + tui.MutedStyle.Render("- "+cmd.Description)
			}
			sugLines = append(sugLines, prefix+style.Render(s)+desc)
		}

		sb.WriteString(sugStyle.Render(strings.Join(sugLines, "\n")))
		sb.WriteString("\n")
	}

	// Help hint
	sb.WriteString("\n")
	sb.WriteString(tui.MutedStyle.Render("Type /help for commands • /quit to exit • Tab for completion"))

	return sb.String()
}

// executeInput parses and executes the user input
func (m *Model) executeInput(input string) (string, error) {
	// Check if it's a slash command
	if cmd, found := strings.CutPrefix(input, "/"); found {
		return m.executeCommand(cmd)
	}

	// Not a command - show help
	return "", fmt.Errorf("unknown input. Type /help for available commands")
}

// executeCommand parses and executes a slash command
func (m *Model) executeCommand(input string) (string, error) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return "", nil
	}

	cmdName := parts[0]
	args := parts[1:]

	cmd := m.registry.Get(cmdName)
	if cmd == nil {
		return "", fmt.Errorf("unknown command: /%s. Type /help for available commands", cmdName)
	}

	// Handle special commands that need REPL context
	switch cmd.Name {
	case "status":
		return m.runExternalCommand("citadel", "status")
	case "services":
		if len(args) == 0 {
			return m.runExternalCommand("citadel", "status")
		}
		// Handle start/stop/restart
		if len(args) >= 2 {
			action := args[0]
			service := args[1]
			switch action {
			case "start":
				return m.runExternalCommand("citadel", "run", "--service", service)
			case "stop":
				return m.runExternalCommand("citadel", "stop", "--service", service)
			case "restart":
				if _, err := m.runExternalCommand("citadel", "stop", "--service", service); err != nil {
					return "", err
				}
				return m.runExternalCommand("citadel", "run", "--service", service)
			}
		}
		return "", fmt.Errorf("usage: /services [start|stop|restart <name>]")
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
		return fmt.Sprintf("Citadel CLI version %s", m.version), nil
	}

	// Use the command's handler
	if cmd.Handler != nil {
		return "", cmd.Handler(args)
	}

	return "", fmt.Errorf("command /%s is not implemented", cmd.Name)
}

// runExternalCommand runs a citadel CLI command and returns its output
func (m *Model) runExternalCommand(name string, args ...string) (string, error) {
	// Get the path to the current executable
	execPath, err := os.Executable()
	if err != nil {
		execPath = name
	}

	cmd := exec.Command(execPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("command failed: %v", err)
	}
	return string(output), nil
}

// Run starts the REPL
func Run(cfg Config) error {
	p := tea.NewProgram(New(cfg))
	_, err := p.Run()
	return err
}
