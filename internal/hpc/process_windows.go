//go:build windows

package hpc

import (
	"io"
	"os"
	"time"
)

// ExecProcess is unavailable on Windows; the hpc profile refuses there.
type ExecProcess struct{}

// StartProcess always fails on Windows.
func StartProcess([]string, []string, io.Writer, bool) (*ExecProcess, error) {
	return nil, ErrUnsupportedPlatform
}

// StopGroup is a no-op on Windows.
func StopGroup(Process, time.Duration) {}

// Done is never reached on Windows.
func (p *ExecProcess) Done() <-chan error { return nil }

// Signal is never reached on Windows.
func (p *ExecProcess) Signal(os.Signal) error { return ErrUnsupportedPlatform }
