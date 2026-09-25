//go:build linux

// internal/services/external_supervisor_linux.go
package services

import (
	"os"
	"path/filepath"
	"strconv"
)

// externalSupervisorUnit reads the target pid's cgroup and this process's own
// cgroup from /proc and delegates the decision to classifyExternalSupervisor.
// Indirected through a var so tests drive StopNativeService's external branch
// without a real /proc read. Reading /proc is Linux-only, exactly like the rest
// of the native-engine process tooling in native_stop.go.
var externalSupervisorUnit = defaultExternalSupervisorUnit

func defaultExternalSupervisorUnit(pid int) (unit string, external bool) {
	target, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", false
	}
	self, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", false
	}
	return classifyExternalSupervisor(string(target), string(self))
}
