//go:build windows

// internal/update/restart_windows.go
// Clean self-restart for Windows after an in-place binary swap.
//
// Windows does not support execve(2), and running executables are file-locked,
// so the swap uses the rename workaround in atomicReplaceWindows. To pick up
// the new binary we exit cleanly and let the supervisor relaunch us: Citadel is
// installed as a Windows service (sc.exe) configured to restart, and the
// service manager will start the new binary. If Citadel is run in the
// foreground without a supervisor, the operator must restart it manually; we
// log that expectation at the call site.
package update

import (
	"os"
	"os/exec"
)

// RestartProcess relaunches the agent onto the new binary. It spawns a fresh
// detached process from the (now-replaced) binary path with the same arguments
// and then exits the current process. Under the Windows service manager the
// service will be restarted regardless; the spawned process covers the
// foreground case. On success this call does not return.
func RestartProcess() error {
	path, err := GetCurrentBinaryPath()
	if err != nil {
		// Fall back to a clean exit so a supervisor can relaunch us.
		os.Exit(0)
		return nil
	}

	cmd := exec.Command(path, os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		// Could not spawn; rely on the service manager to relaunch.
		os.Exit(0)
		return nil
	}

	// Detach and exit so the old image releases any locks; the new process (or
	// service manager) continues on the updated binary.
	_ = cmd.Process.Release()
	os.Exit(0)
	return nil // unreachable
}
