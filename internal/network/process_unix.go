//go:build !windows

// internal/network/process_unix.go
package network

import (
	"os"
	"syscall"
)

// processAlive reports whether pid names a live process. Signal 0 performs
// the permission and existence checks without delivering anything, so this
// also answers true for a process owned by another user (a root-owned
// `citadel up` seen from an unprivileged `citadel work`) — which is exactly
// the case that matters here.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || err == syscall.EPERM
}
