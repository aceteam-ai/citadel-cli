// internal/ui/spinner.go
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
)

// SpinnerStyle defines the visual style of the spinner
type SpinnerStyle int

const (
	// StyleThinking shows a pulsing dot for "thinking" states
	StyleThinking SpinnerStyle = iota
	// StyleWorking shows an active spinner for "working" states
	StyleWorking
	// StyleDoing shows a progress indicator for "doing" states
	StyleDoing
)

var (
	thinkingFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	workingFrames  = []string{"◐", "◓", "◑", "◒"}
	doingFrames    = []string{"▹▹▹▹▹", "▸▹▹▹▹", "▹▸▹▹▹", "▹▹▸▹▹", "▹▹▹▸▹", "▹▹▹▹▸"}
)

// Spinner provides Claude Code-style status updates
type Spinner struct {
	mu        sync.Mutex
	style     SpinnerStyle
	message   string
	detail    string
	running   bool
	done      chan struct{}
	writer    io.Writer
	startTime time.Time
	showTime  bool
}

// NewSpinner creates a new spinner with the given style
func NewSpinner(style SpinnerStyle) *Spinner {
	return &Spinner{
		style:    style,
		writer:   os.Stdout,
		showTime: true,
	}
}

// SetWriter sets the output writer
func (s *Spinner) SetWriter(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writer = w
}

// SetShowTime enables/disables elapsed time display
func (s *Spinner) SetShowTime(show bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.showTime = show
}

// Start begins the spinner animation with a message
func (s *Spinner) Start(message string) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.message = message
	s.detail = ""
	s.running = true
	s.done = make(chan struct{})
	s.startTime = time.Now()
	s.mu.Unlock()

	go s.animate()
}

// UpdateMessage changes the spinner message while running
func (s *Spinner) UpdateMessage(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.message = message
}

// UpdateDetail sets additional detail text (shown after message)
func (s *Spinner) UpdateDetail(detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detail = detail
}

// Stop stops the spinner and optionally shows a final message
func (s *Spinner) Stop(finalMessage string) {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.done)
	s.mu.Unlock()

	// Clear the line and print final message
	s.clearLine()
	if finalMessage != "" {
		fmt.Fprintln(s.writer, finalMessage)
	}
}

// Success stops with a green checkmark
func (s *Spinner) Success(message string) {
	s.Stop(color.GreenString("✓") + " " + message)
}

// Fail stops with a red X
func (s *Spinner) Fail(message string) {
	s.Stop(color.RedString("✗") + " " + message)
}

// Warning stops with a yellow warning
func (s *Spinner) Warning(message string) {
	s.Stop(color.YellowString("⚠") + " " + message)
}

func (s *Spinner) animate() {
	frames := s.getFrames()
	frameIndex := 0
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mu.Lock()
			frame := frames[frameIndex%len(frames)]
			message := s.message
			detail := s.detail
			elapsed := time.Since(s.startTime)
			showTime := s.showTime
			style := s.style
			s.mu.Unlock()

			s.clearLine()
			s.renderFrame(frame, message, detail, elapsed, showTime, style)
			frameIndex++
		}
	}
}

func (s *Spinner) getFrames() []string {
	switch s.style {
	case StyleThinking:
		return thinkingFrames
	case StyleWorking:
		return workingFrames
	case StyleDoing:
		return doingFrames
	default:
		return thinkingFrames
	}
}

func (s *Spinner) clearLine() {
	fmt.Fprint(s.writer, "\r\033[K")
}

func (s *Spinner) renderFrame(frame, message, detail string, elapsed time.Duration, showTime bool, style SpinnerStyle) {
	var prefix string
	switch style {
	case StyleThinking:
		prefix = color.CyanString(frame)
	case StyleWorking:
		prefix = color.YellowString(frame)
	case StyleDoing:
		prefix = color.GreenString(frame)
	}

	var timeStr string
	if showTime && elapsed > time.Second {
		timeStr = color.HiBlackString(" (%s)", formatDuration(elapsed))
	}

	var detailStr string
	if detail != "" {
		detailStr = color.HiBlackString(" %s", detail)
	}

	fmt.Fprintf(s.writer, "%s %s%s%s", prefix, message, detailStr, timeStr)
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%ds", minutes, seconds)
}

// StatusLine provides a simple one-line status update without animation
type StatusLine struct {
	writer io.Writer
}

// NewStatusLine creates a new status line writer
func NewStatusLine() *StatusLine {
	return &StatusLine{writer: os.Stdout}
}

// Thinking prints a thinking status
func (sl *StatusLine) Thinking(message string) {
	fmt.Fprintf(sl.writer, "%s %s\n", color.CyanString("◆"), message)
}

// Working prints a working status
func (sl *StatusLine) Working(message string) {
	fmt.Fprintf(sl.writer, "%s %s\n", color.YellowString("◆"), message)
}

// Doing prints a doing status
func (sl *StatusLine) Doing(message string) {
	fmt.Fprintf(sl.writer, "%s %s\n", color.GreenString("◆"), message)
}

// Success prints a success status
func (sl *StatusLine) Success(message string) {
	fmt.Fprintf(sl.writer, "%s %s\n", color.GreenString("✓"), message)
}

// Fail prints a failure status
func (sl *StatusLine) Fail(message string) {
	fmt.Fprintf(sl.writer, "%s %s\n", color.RedString("✗"), message)
}

// Warning prints a warning status
func (sl *StatusLine) Warning(message string) {
	fmt.Fprintf(sl.writer, "%s %s\n", color.YellowString("⚠"), message)
}

// Info prints an info status
func (sl *StatusLine) Info(message string) {
	fmt.Fprintf(sl.writer, "%s %s\n", color.BlueString("ℹ"), message)
}

// Step prints a step in a process (e.g., "Step 1/5: Installing...")
func (sl *StatusLine) Step(current, total int, message string) {
	progress := color.HiBlackString("[%d/%d]", current, total)
	fmt.Fprintf(sl.writer, "%s %s %s\n", color.CyanString("▸"), progress, message)
}

// ProgressBar shows a simple progress bar
type ProgressBar struct {
	mu      sync.Mutex
	writer  io.Writer
	total   int
	current int
	message string
	width   int
}

// NewProgressBar creates a new progress bar
func NewProgressBar(total int, message string) *ProgressBar {
	return &ProgressBar{
		writer:  os.Stdout,
		total:   total,
		message: message,
		width:   30,
	}
}

// Update updates the progress bar
func (pb *ProgressBar) Update(current int) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.current = current
	pb.render()
}

// Increment increases progress by 1
func (pb *ProgressBar) Increment() {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.current++
	pb.render()
}

// Finish completes the progress bar
func (pb *ProgressBar) Finish() {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.current = pb.total
	pb.render()
	fmt.Fprintln(pb.writer)
}

func (pb *ProgressBar) render() {
	percent := float64(pb.current) / float64(pb.total)
	filled := int(percent * float64(pb.width))
	if filled > pb.width {
		filled = pb.width
	}

	bar := strings.Repeat("█", filled) + strings.Repeat("░", pb.width-filled)
	fmt.Fprintf(pb.writer, "\r%s %s %s %d%%",
		color.CyanString("▸"),
		pb.message,
		color.HiBlackString("[%s]", bar),
		int(percent*100))
}

// RunWithSpinner executes a function while showing a spinner
func RunWithSpinner(style SpinnerStyle, message string, fn func() error) error {
	spinner := NewSpinner(style)
	spinner.Start(message)
	err := fn()
	if err != nil {
		spinner.Fail(message + " - failed")
		return err
	}
	spinner.Success(message)
	return nil
}
