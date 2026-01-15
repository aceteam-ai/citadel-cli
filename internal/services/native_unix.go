//go:build !windows

// internal/services/native_unix.go
package services

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr sets Unix-specific process attributes
func setSysProcAttr(cmd *exec.Cmd) {
	// Start in new process group so it survives parent exit
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}
