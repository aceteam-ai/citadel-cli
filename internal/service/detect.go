//go:build linux

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ManagedUnit identifies a citadel-managed systemd unit found ACTIVE on this
// host right now.
//
// This is deliberately independent of which process is asking. Unlike
// managedByServiceManager (cmd/agent_tools.go), which checks INVOCATION_ID to
// answer "is *this* process running under systemd", ActiveManagedUnit
// inspects the units on disk and their live systemctl state directly -- so it
// still gives the right answer from a short-lived, unrelated CLI invocation,
// e.g. an operator's `citadel update install` run over SSH in a plain login
// shell that never inherited the worker's own service-manager environment
// (citadel#454: the binary swap succeeded but the separately-running managed
// worker process kept executing the pre-swap code indefinitely).
type ManagedUnit struct {
	Name     string // unit name without ".service", e.g. "citadel-worker", "citadel"
	UserMode bool
}

// Description returns a human-readable label for warnings/logs.
func (u ManagedUnit) Description() string {
	if u.UserMode {
		return fmt.Sprintf("%s.service (user service)", u.Name)
	}
	return fmt.Sprintf("%s.service (system service)", u.Name)
}

// RestartCommand returns the exact shell command an operator can run to
// restart this unit.
func (u ManagedUnit) RestartCommand() string {
	if u.UserMode {
		return fmt.Sprintf("systemctl --user restart %s", u.Name)
	}
	return fmt.Sprintf("sudo systemctl restart %s", u.Name)
}

// Restart restarts the unit via systemctl so the process currently running
// picks up the just-installed binary. System units require root, matching
// every other systemd.go mutation (Start/Stop/Install).
func (u ManagedUnit) Restart() error {
	if !u.UserMode && os.Geteuid() != 0 {
		return fmt.Errorf("restarting %s requires root; run: %s", u.Name, u.RestartCommand())
	}
	ctl := systemctlArgs(u.UserMode)
	return runCmd("systemctl", append(ctl, "restart", u.Name)...)
}

// ActiveManagedUnit reports whether any citadel-managed systemd unit is
// currently active -- either the install.sh/packer fleet unit
// (citadel-worker.service, the actual production deployment path) or a unit
// installed via `citadel service install` (citadel.service, system or user).
//
// It reuses the exact enumeration and ownership check RematerializeManagedUnits
// already relies on (candidateManagedUnits, isCitadelManagedUnit), so it
// recognizes precisely the units that function already treats as
// citadel-owned -- no new "is this ours" heuristic. This is also why a plain
// service.Manager.Status() (used by `citadel service status`) is not enough
// on its own: that call only ever looks at citadel.service and has no
// awareness of the fleet's citadel-worker.service at all.
func ActiveManagedUnit() (ManagedUnit, bool) {
	for _, cand := range candidateManagedUnits() {
		data, err := os.ReadFile(cand.path)
		if err != nil {
			continue // not present on this host
		}
		if !isCitadelManagedUnit(string(data)) {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(cand.path), ".service")
		ctl := systemctlArgs(cand.userMode)
		out, err := exec.Command("systemctl", append(ctl, "is-active", name)...).Output()
		if err != nil {
			continue // not loaded / systemctl error -- treat as not active
		}
		if strings.TrimSpace(string(out)) == "active" {
			return ManagedUnit{Name: name, UserMode: cand.userMode}, true
		}
	}
	return ManagedUnit{}, false
}
