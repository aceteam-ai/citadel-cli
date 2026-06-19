//go:build !windows

// internal/update/restart_unix.go
// Clean self-restart for Unix-like systems after an in-place binary swap.
package update

import (
	"fmt"
	"os"
	"syscall"
)

// RestartProcess re-execs the current process onto the freshly-installed binary
// using execve(2). The PID is preserved and the new binary takes over the
// process image, so this works both for foreground runs and under a supervisor
// (systemd Restart=always). On success this call does not return.
func RestartProcess() error {
	path, err := GetCurrentBinaryPath()
	if err != nil {
		return fmt.Errorf("resolve current binary: %w", err)
	}
	// os.Args[0] may be a relative/symlinked name; exec the resolved path but
	// keep the original argv so the new process sees the same flags.
	argv := os.Args
	if err := syscall.Exec(path, argv, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", path, err)
	}
	return nil // unreachable on success
}
