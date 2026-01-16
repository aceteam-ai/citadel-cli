// internal/terminal/session.go
//go:build !windows

package terminal

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Session represents a terminal session with a PTY
type Session struct {
	// ID is the unique session identifier
	ID string

	// UserID is the user who owns this session
	UserID string

	// OrgID is the organization this session belongs to
	OrgID string

	// cmd is the running shell process
	cmd *exec.Cmd

	// pty is the pseudo-terminal file
	pty *os.File

	// cols is the current number of columns
	cols uint16

	// rows is the current number of rows
	rows uint16

	// lastActive is the timestamp of the last activity
	lastActive time.Time

	// mu protects concurrent access
	mu sync.RWMutex

	// closed indicates if the session has been closed
	closed bool

	// onClose is called when the session is closed
	onClose func()
}

// SessionConfig holds the configuration for creating a session
type SessionConfig struct {
	// ID is the unique session identifier
	ID string

	// UserID is the user who owns this session
	UserID string

	// OrgID is the organization this session belongs to
	OrgID string

	// Shell is the shell command to run
	Shell string

	// InitialCols is the initial number of columns (default 80)
	InitialCols uint16

	// InitialRows is the initial number of rows (default 24)
	InitialRows uint16

	// Env is additional environment variables
	Env []string

	// OnClose is called when the session is closed
	OnClose func()
}

// NewSession creates a new terminal session with a PTY
func NewSession(config SessionConfig) (*Session, error) {
	// Set defaults
	if config.InitialCols == 0 {
		config.InitialCols = 80
	}
	if config.InitialRows == 0 {
		config.InitialRows = 24
	}

	// Check if the shell exists
	if _, err := exec.LookPath(config.Shell); err != nil {
		return nil, ErrShellNotFound
	}

	// Create the command
	cmd := exec.Command(config.Shell)

	// Set up environment
	cmd.Env = append(os.Environ(), config.Env...)
	cmd.Env = append(cmd.Env,
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	)

	// Start the command with a PTY
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: config.InitialCols,
		Rows: config.InitialRows,
	})
	if err != nil {
		return nil, ErrPTYCreationFailed
	}

	session := &Session{
		ID:         config.ID,
		UserID:     config.UserID,
		OrgID:      config.OrgID,
		cmd:        cmd,
		pty:        ptmx,
		cols:       config.InitialCols,
		rows:       config.InitialRows,
		lastActive: time.Now(),
		onClose:    config.OnClose,
	}

	return session, nil
}

// Read reads from the PTY
func (s *Session) Read(p []byte) (n int, err error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return 0, ErrConnectionClosed
	}
	s.mu.RUnlock()

	n, err = s.pty.Read(p)
	if err == nil && n > 0 {
		s.updateLastActive()
	}
	return n, err
}

// Write writes to the PTY
func (s *Session) Write(p []byte) (n int, err error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return 0, ErrConnectionClosed
	}
	s.mu.RUnlock()

	n, err = s.pty.Write(p)
	if err == nil && n > 0 {
		s.updateLastActive()
	}
	return n, err
}

// Resize changes the PTY dimensions
func (s *Session) Resize(cols, rows uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrConnectionClosed
	}

	if cols == 0 || rows == 0 {
		return ErrInvalidResize
	}

	err := pty.Setsize(s.pty, &pty.Winsize{
		Cols: cols,
		Rows: rows,
	})
	if err != nil {
		return err
	}

	s.cols = cols
	s.rows = rows
	s.lastActive = time.Now()

	return nil
}

// Size returns the current PTY dimensions
func (s *Session) Size() (cols, rows uint16) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cols, s.rows
}

// LastActive returns the last activity timestamp
func (s *Session) LastActive() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastActive
}

// updateLastActive updates the last activity timestamp
func (s *Session) updateLastActive() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

// Close terminates the session
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	// Close the PTY file
	if s.pty != nil {
		s.pty.Close()
	}

	// Try graceful shutdown first
	if s.cmd != nil && s.cmd.Process != nil {
		// Send SIGHUP
		s.cmd.Process.Signal(syscall.SIGHUP)

		// Give the process a moment to exit
		done := make(chan struct{})
		go func() {
			s.cmd.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Process exited cleanly
		case <-time.After(2 * time.Second):
			// Force kill if it doesn't exit
			s.cmd.Process.Kill()
			s.cmd.Wait()
		}
	}

	// Call the onClose callback
	if s.onClose != nil {
		s.onClose()
	}

	return nil
}

// IsClosed returns whether the session is closed
func (s *Session) IsClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

// PTY returns the underlying PTY file (for advanced use)
func (s *Session) PTY() io.ReadWriter {
	return s.pty
}

// SessionManager manages multiple terminal sessions
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	maxCount int
}

// NewSessionManager creates a new session manager
func NewSessionManager(maxSessions int) *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*Session),
		maxCount: maxSessions,
	}
}

// Add adds a session to the manager
func (m *SessionManager) Add(session *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.sessions) >= m.maxCount {
		return ErrMaxConnectionsReached
	}

	if _, exists := m.sessions[session.ID]; exists {
		return ErrSessionAlreadyExists
	}

	m.sessions[session.ID] = session
	return nil
}

// Get retrieves a session by ID
func (m *SessionManager) Get(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	session, ok := m.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return session, nil
}

// Remove removes a session from the manager
func (m *SessionManager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// Count returns the number of active sessions
func (m *SessionManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// CloseAll closes all sessions
func (m *SessionManager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	// Close sessions outside the lock
	for _, s := range sessions {
		s.Close()
	}
}

// CloseIdle closes sessions that have been idle for longer than the timeout
func (m *SessionManager) CloseIdle(timeout time.Duration) int {
	m.mu.Lock()
	var toClose []*Session
	cutoff := time.Now().Add(-timeout)

	for _, s := range m.sessions {
		if s.LastActive().Before(cutoff) {
			toClose = append(toClose, s)
			delete(m.sessions, s.ID)
		}
	}
	m.mu.Unlock()

	// Close sessions outside the lock
	for _, s := range toClose {
		s.Close()
	}

	return len(toClose)
}
