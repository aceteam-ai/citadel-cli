package whimsy

import (
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/aceteam-ai/citadel-cli/internal/tui"
)

// SpinnerModel is a BubbleTea model for an animated spinner with rotating messages
type SpinnerModel struct {
	spinner       spinner.Model
	messages      []string
	currentMsg    int
	quitting      bool
	err           error
	result        string
	done          bool
	messageRotate time.Duration
	lastRotate    time.Time
}

// SpinnerOption configures the spinner
type SpinnerOption func(*SpinnerModel)

// WithMessages sets custom messages for the spinner
func WithMessages(messages []string) SpinnerOption {
	return func(s *SpinnerModel) {
		if len(messages) > 0 {
			s.messages = messages
		}
	}
}

// WithRotateInterval sets how often messages rotate
func WithRotateInterval(d time.Duration) SpinnerOption {
	return func(s *SpinnerModel) {
		s.messageRotate = d
	}
}

// NewSpinner creates a new whimsy spinner with optional configuration
func NewSpinner(opts ...SpinnerOption) SpinnerModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(tui.ColorPrimary)

	m := SpinnerModel{
		spinner:       s,
		messages:      ThinkingMessages,
		currentMsg:    rand.IntN(len(ThinkingMessages)),
		messageRotate: 2 * time.Second,
		lastRotate:    time.Now(),
	}

	for _, opt := range opts {
		opt(&m)
	}

	return m
}

// tickMsg is sent periodically to update the spinner
type tickMsg time.Time

// doneMsg signals the spinner should stop
type doneMsg struct {
	result string
	err    error
}

// Done creates a command to stop the spinner with a result
func Done(result string) tea.Cmd {
	return func() tea.Msg {
		return doneMsg{result: result}
	}
}

// Error creates a command to stop the spinner with an error
func Error(err error) tea.Cmd {
	return func() tea.Msg {
		return doneMsg{err: err}
	}
}

func (m SpinnerModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m SpinnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.quitting = true
			return m, tea.Quit
		}

	case tickMsg:
		// Check if it's time to rotate the message
		if time.Since(m.lastRotate) >= m.messageRotate {
			m.currentMsg = (m.currentMsg + 1) % len(m.messages)
			m.lastRotate = time.Now()
		}
		return m, tickCmd()

	case doneMsg:
		m.done = true
		m.result = msg.result
		m.err = msg.err
		return m, tea.Quit

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m SpinnerModel) View() string {
	if m.done {
		if m.err != nil {
			return tui.ErrorStyle.Render("✗ ") + m.err.Error() + "\n"
		}
		if m.result != "" {
			return tui.SuccessStyle.Render("✓ ") + m.result + "\n"
		}
		return ""
	}

	if m.quitting {
		return tui.WarningStyle.Render("Interrupted") + "\n"
	}

	return m.spinner.View() + " " + tui.SpinnerStyle.Render(m.messages[m.currentMsg]) + "\n"
}

// Result returns the result after the spinner is done
func (m SpinnerModel) Result() string {
	return m.result
}

// Err returns any error after the spinner is done
func (m SpinnerModel) Err() error {
	return m.err
}

// RunSpinner runs a spinner while executing a function
// This is a convenience function for simple use cases
func RunSpinner(title string, messages []string, fn func() (string, error)) error {
	if !tui.IsTTY() {
		// Non-interactive mode: just run the function with simple output
		fmt.Printf("%s\n", title)
		result, err := fn()
		if err != nil {
			fmt.Printf("✗ %v\n", err)
			return err
		}
		if result != "" {
			fmt.Printf("✓ %s\n", result)
		}
		return nil
	}

	// Create the spinner model
	opts := []SpinnerOption{}
	if len(messages) > 0 {
		opts = append(opts, WithMessages(messages))
	}
	model := NewSpinner(opts...)

	// Channel to receive the function result
	resultCh := make(chan struct {
		result string
		err    error
	})

	// Run the function in a goroutine
	go func() {
		result, err := fn()
		resultCh <- struct {
			result string
			err    error
		}{result, err}
	}()

	// Create the program
	p := tea.NewProgram(model)

	// Wait for the function to complete in another goroutine
	go func() {
		res := <-resultCh
		if res.err != nil {
			p.Send(doneMsg{err: res.err})
		} else {
			p.Send(doneMsg{result: res.result})
		}
	}()

	// Run the TUI
	finalModel, err := p.Run()
	if err != nil {
		return err
	}

	// Check the final model for errors
	if m, ok := finalModel.(SpinnerModel); ok {
		if m.err != nil {
			return m.err
		}
	}

	return nil
}

// RunSpinnerSimple runs a spinner with default thinking messages
func RunSpinnerSimple(title string, fn func() error) error {
	return RunSpinner(title, nil, func() (string, error) {
		err := fn()
		if err != nil {
			return "", err
		}
		return title, nil
	})
}
