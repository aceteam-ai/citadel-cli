//go:build windows

// internal/services/native_windows.go
package services

import (
	"os/exec"
)

// setSysProcAttr sets Windows-specific process attributes
func setSysProcAttr(cmd *exec.Cmd) {
	// Windows doesn't support Setpgid, but processes naturally
	// survive parent exit when started normally
}
