//go:build !linux

package service

import "fmt"

// ManagedUnit mirrors the Linux type for cross-platform callers. Unused on
// this platform: launchd (macOS) and the Windows SCM don't have an
// install.sh-style raw unit outside the service.Manager abstraction, so
// detection there goes entirely through Manager.Status() (see cmd/update.go).
// ActiveManagedUnit below never returns a found ManagedUnit on this platform,
// so these methods are unreachable in practice -- they return loud errors
// rather than nil/"" so a future caller that DOES construct one directly does
// not get a silent, do-nothing "success".
type ManagedUnit struct {
	Name     string
	UserMode bool
}

func (u ManagedUnit) Description() string { return u.Name }

func (u ManagedUnit) RestartCommand() string {
	return "citadel service stop && citadel service start"
}

func (u ManagedUnit) Restart() error {
	return fmt.Errorf("systemd unit restart is not supported on this platform; use `citadel service stop && citadel service start` instead")
}

// ActiveManagedUnit is a no-op on non-Linux platforms (systemd-unit-specific
// scanning). It always reports "not found" so callers fall back to the
// cross-platform service.Manager.Status() check.
func ActiveManagedUnit() (ManagedUnit, bool) {
	return ManagedUnit{}, false
}
