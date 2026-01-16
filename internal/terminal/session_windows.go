// internal/terminal/session_windows.go
//go:build windows

package terminal

import (
	"errors"
	"io"
	"sync"
	"time"
)

// ErrWindowsNotSupported is returned when PTY operations are attempted on Windows
var ErrWindowsNotSupported = errors.New("PTY terminal sessions are not yet supported on Windows")

// Session represents a terminal session with a PTY
// Note: PTY support on Windows requires ConPTY which is not yet implemented
type Session struct {
	ID         string
	UserID     string
	OrgID      string
	cols       uint16
	rows       uint16
	lastActive time.Time
	mu         sync.RWMutex
	closed     bool
	onClose    func()
}

// SessionConfig holds the configuration for creating a session
type SessionConfig struct {
	ID          string
	UserID      string
	OrgID       string
	Shell       string
	InitialCols uint16
	InitialRows uint16
	Env         []string
	OnClose     func()
}

// NewSession creates a new terminal session with a PTY
// Note: This is not supported on Windows
func NewSession(config SessionConfig) (*Session, error) {
	return nil, ErrWindowsNotSupported
}

// Read reads from the PTY
func (s *Session) Read(p []byte) (n int, err error) {
	return 0, ErrWindowsNotSupported
}

// Write writes to the PTY
func (s *Session) Write(p []byte) (n int, err error) {
	return 0, ErrWindowsNotSupported
}

// Resize changes the PTY dimensions
func (s *Session) Resize(cols, rows uint16) error {
	return ErrWindowsNotSupported
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

// Close terminates the session
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

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
	return nil
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

	for _, s := range toClose {
		s.Close()
	}

	return len(toClose)
}
