package whimsy

import (
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/aceteam-ai/citadel-cli/internal/tui"
)

// SimpleSpinner is a lightweight spinner for non-BubbleTea contexts.
// It runs in a goroutine and can be started/stopped easily.
type SimpleSpinner struct {
	messages      []string
	currentMsg    int
	frames        []string
	frameIndex    int
	interval      time.Duration
	msgInterval   time.Duration
	stopCh        chan struct{}
	doneCh        chan struct{}
	mu            sync.Mutex
	running       bool
	lastMsgChange time.Time
}

// NewSimpleSpinner creates a new simple spinner
func NewSimpleSpinner(messages []string) *SimpleSpinner {
	if len(messages) == 0 {
		messages = ThinkingMessages
	}
	return &SimpleSpinner{
		messages:    messages,
		currentMsg:  rand.IntN(len(messages)),
		frames:      []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		interval:    80 * time.Millisecond,
		msgInterval: 2 * time.Second,
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
}

// Start begins the spinner animation
func (s *SimpleSpinner) Start() {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		// Non-TTY: don't start animation
		return
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.lastMsgChange = time.Now()
	s.mu.Unlock()

	go s.run()
}

func (s *SimpleSpinner) run() {
	defer close(s.doneCh)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	// Hide cursor
	fmt.Print("\033[?25l")

	for {
		select {
		case <-s.stopCh:
			// Clear the line and show cursor
			fmt.Print("\r\033[K\033[?25h")
			return
		case <-ticker.C:
			s.mu.Lock()

			// Rotate message if interval passed
			if time.Since(s.lastMsgChange) >= s.msgInterval {
				s.currentMsg = (s.currentMsg + 1) % len(s.messages)
				s.lastMsgChange = time.Now()
			}

			// Advance frame
			s.frameIndex = (s.frameIndex + 1) % len(s.frames)
			frame := s.frames[s.frameIndex]
			msg := s.messages[s.currentMsg]

			s.mu.Unlock()

			// Print the spinner
			fmt.Printf("\r\033[K%s %s", tui.SpinnerStyle.Render(frame), tui.SpinnerStyle.Render(msg))
		}
	}
}

// Stop stops the spinner and optionally prints a result
func (s *SimpleSpinner) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	s.mu.Unlock()

	close(s.stopCh)
	<-s.doneCh
}

// StopWithSuccess stops the spinner and prints a success message
func (s *SimpleSpinner) StopWithSuccess(msg string) {
	s.Stop()
	if term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Printf("\r\033[K%s %s\n", tui.SuccessStyle.Render("✓"), msg)
	} else {
		fmt.Printf("✓ %s\n", msg)
	}
}

// StopWithError stops the spinner and prints an error message
func (s *SimpleSpinner) StopWithError(msg string) {
	s.Stop()
	if term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Printf("\r\033[K%s %s\n", tui.ErrorStyle.Render("✗"), msg)
	} else {
		fmt.Printf("✗ %s\n", msg)
	}
}

// StopWithWarning stops the spinner and prints a warning message
func (s *SimpleSpinner) StopWithWarning(msg string) {
	s.Stop()
	if term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Printf("\r\033[K%s %s\n", tui.WarningStyle.Render("⚠"), msg)
	} else {
		fmt.Printf("⚠ %s\n", msg)
	}
}

// WithSpinner runs a function with a spinner displayed
func WithSpinner(messages []string, fn func() error) error {
	spinner := NewSimpleSpinner(messages)
	spinner.Start()
	err := fn()
	if err != nil {
		spinner.StopWithError(err.Error())
		return err
	}
	spinner.Stop()
	return nil
}

// WithSpinnerResult runs a function with a spinner and displays a result message
func WithSpinnerResult(messages []string, fn func() (string, error)) error {
	spinner := NewSimpleSpinner(messages)
	spinner.Start()
	result, err := fn()
	if err != nil {
		spinner.StopWithError(err.Error())
		return err
	}
	if result != "" {
		spinner.StopWithSuccess(result)
	} else {
		spinner.Stop()
	}
	return nil
}
