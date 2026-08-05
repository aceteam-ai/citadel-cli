//go:build windows

// internal/network/process_windows.go
package network

import "os"

// processAlive reports whether pid names a live process. On Windows
// os.FindProcess actually opens the process handle (unlike unix, where it
// always succeeds), so a successful lookup is itself the liveness answer.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	proc.Release()
	return true
}
