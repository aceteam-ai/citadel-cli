// internal/console/pty_windows.go
//go:build windows

package console

import (
	"errors"
	"io"
)

// errUnsupported is returned on Windows where the embedded console PTY is not
// yet supported. The TUI Console tab should be hidden on this platform.
var errUnsupported = errors.New("console: embedded PTY is not supported on Windows")

// PTYSession is the Windows stub. All methods return errUnsupported.
type PTYSession struct{}

// PTYConfig holds the configuration for creating a PTYSession.
type PTYConfig struct {
	Shell       string
	InitialCols uint16
	InitialRows uint16
	Env         []string
	OnClose     func()
}

// NewPTYSession always returns an error on Windows.
func NewPTYSession(_ PTYConfig) (*PTYSession, error) {
	return nil, errUnsupported
}

// Read always returns an error on Windows.
func (s *PTYSession) Read(_ []byte) (int, error) {
	return 0, io.EOF
}

// Write always returns an error on Windows.
func (s *PTYSession) Write(_ []byte) (int, error) {
	return 0, io.ErrClosedPipe
}

// Resize always returns an error on Windows.
func (s *PTYSession) Resize(_, _ uint16) error {
	return errUnsupported
}

// Size returns zero dimensions on Windows.
func (s *PTYSession) Size() (cols, rows uint16) {
	return 0, 0
}

// Close is a no-op on Windows.
func (s *PTYSession) Close() error {
	return nil
}

// IsClosed always returns true on Windows.
func (s *PTYSession) IsClosed() bool {
	return true
}
