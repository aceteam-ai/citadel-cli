// internal/ui/interactive.go
package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/fatih/color"
)

// --- Generic Selector Component ---

type selectorModel struct {
	question string
	cursor   int
	choices  []string
	choice   string
}

func (m selectorModel) Init() tea.Cmd {
	return nil
}

func (m selectorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit

		case "enter":
			m.choice = m.choices[m.cursor]
			return m, tea.Quit

		case "down", "j":
			m.cursor++
			if m.cursor >= len(m.choices) {
				m.cursor = 0
			}

		case "up", "k":
			m.cursor--
			if m.cursor < 0 {
				m.cursor = len(m.choices) - 1
			}
		}
	}
	return m, nil
}

func (m selectorModel) View() string {
	var sb strings.Builder
	sb.WriteString(m.question + "\n\n")

	for i, choice := range m.choices {
		cursor := "  "
		if m.cursor == i {
			cursor = color.CyanString("> ")
		}
		sb.WriteString(fmt.Sprintf("%s%s\n", cursor, choice))
	}

	sb.WriteString("\n(Use arrow keys to navigate, enter to select, q to quit)\n")
	return sb.String()
}

// --- Text Input Component ---

type textInputModel struct {
	question  string
	textInput textinput.Model
	err       error
}

func newTextInput(question, placeholder, defaultValue string) textInputModel {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.SetValue(defaultValue)
	ti.Focus()
	ti.CharLimit = 156
	ti.Width = 50

	return textInputModel{
		question:  question,
		textInput: ti,
		err:       nil,
	}
}

func (m textInputModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m textInputModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEnter, tea.KeyCtrlC, tea.KeyEsc:
			return m, tea.Quit
		}
	}

	m.textInput, cmd = m.textInput.Update(msg)
	return m, cmd
}

func (m textInputModel) View() string {
	return fmt.Sprintf(
		"%s\n\n%s\n\n(esc to quit)",
		m.question,
		m.textInput.View(),
	)
}

// --- Public Functions to Run Prompts ---

// AskSelect presents the user with a list of choices and returns the selected one.
func AskSelect(question string, choices []string) (string, error) {
	p := tea.NewProgram(selectorModel{question: question, choices: choices})
	m, err := p.Run()
	if err != nil {
		return "", err
	}

	result := m.(selectorModel).choice
	if result == "" {
		return "", fmt.Errorf("no option selected")
	}
	return result, nil
}

// AskInput presents the user with a text input field.
func AskInput(question, placeholder, defaultValue string) (string, error) {
	p := tea.NewProgram(newTextInput(question, placeholder, defaultValue))
	m, err := p.Run()
	if err != nil {
		return "", err
	}

	result := m.(textInputModel).textInput.Value()
	if result == "" {
		// Allow empty input if a default value was present
		if defaultValue != "" {
			return defaultValue, nil
		}
		return "", fmt.Errorf("input cannot be empty")
	}
	return result, nil
}
