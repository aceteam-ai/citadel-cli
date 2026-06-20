// internal/console/pty_unix.go
//go:build !windows

package console

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// PTYSession manages an embedded PTY for the TUI Console tab.
// Unlike terminal.Session (which serves remote WebSocket clients),
// PTYSession is owned by the TUI and streamed to viewers via Streamer.
type PTYSession struct {
	cmd  *exec.Cmd
	ptmx *os.File

	cols uint16
	rows uint16

	mu     sync.RWMutex
	closed bool

	onClose func()
}

// PTYConfig holds the configuration for creating a PTYSession.
type PTYConfig struct {
	Shell       string
	InitialCols uint16
	InitialRows uint16
	Env         []string
	OnClose     func()
}

// NewPTYSession spawns a shell in a new PTY.
func NewPTYSession(cfg PTYConfig) (*PTYSession, error) {
	if cfg.InitialCols == 0 {
		cfg.InitialCols = 80
	}
	if cfg.InitialRows == 0 {
		cfg.InitialRows = 24
	}

	shell := cfg.Shell
	if shell == "" {
		if s := os.Getenv("SHELL"); s != "" {
			shell = s
		} else {
			shell = "/bin/bash"
		}
	}

	if _, err := exec.LookPath(shell); err != nil {
		return nil, errors.New("console: shell not found: " + shell)
	}

	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(), cfg.Env...)
	cmd.Env = append(cmd.Env,
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	)

	size := &pty.Winsize{
		Cols: cfg.InitialCols,
		Rows: cfg.InitialRows,
	}

	ptmx, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return nil, errors.New("console: failed to create PTY: " + err.Error())
	}

	return &PTYSession{
		cmd:     cmd,
		ptmx:    ptmx,
		cols:    cfg.InitialCols,
		rows:    cfg.InitialRows,
		onClose: cfg.OnClose,
	}, nil
}

// Read reads output from the PTY. Blocks until data is available.
func (s *PTYSession) Read(p []byte) (int, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return 0, io.EOF
	}
	s.mu.RUnlock()

	return s.ptmx.Read(p)
}

// Write sends input to the PTY.
func (s *PTYSession) Write(p []byte) (int, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return 0, io.ErrClosedPipe
	}
	s.mu.RUnlock()

	return s.ptmx.Write(p)
}

// Resize changes the PTY dimensions.
func (s *PTYSession) Resize(cols, rows uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return io.ErrClosedPipe
	}
	if cols == 0 || rows == 0 {
		return errors.New("console: invalid resize dimensions")
	}

	if err := pty.Setsize(s.ptmx, &pty.Winsize{Cols: cols, Rows: rows}); err != nil {
		return err
	}
	s.cols = cols
	s.rows = rows
	return nil
}

// Size returns the current PTY dimensions.
func (s *PTYSession) Size() (cols, rows uint16) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cols, s.rows
}

// Close terminates the shell and closes the PTY.
func (s *PTYSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	// Signal the process to exit
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGHUP)

		done := make(chan struct{})
		go func() {
			_ = s.cmd.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = s.cmd.Process.Kill()
			<-done
		}
	}

	err := s.ptmx.Close()

	if s.onClose != nil {
		s.onClose()
	}

	return err
}

// IsClosed returns whether the session has been closed.
func (s *PTYSession) IsClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}
