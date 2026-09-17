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

// EnableNowCommand returns the exact shell command that enables the unit and
// starts it immediately (and on future boots). Mirrors RestartCommand's format
// so callers can print a consistent, copy-pasteable next step.
func (u ManagedUnit) EnableNowCommand() string {
	if u.UserMode {
		return fmt.Sprintf("systemctl --user enable --now %s", u.Name)
	}
	return fmt.Sprintf("sudo systemctl enable --now %s", u.Name)
}

// EnableNow enables and starts the unit immediately (`systemctl enable --now`),
// so an already-installed-but-not-running citadel worker unit begins serving
// now and on future boots.
//
// `enable --now` on an ALREADY-active unit does NOT restart it -- it is a
// no-op for the running process, dropping no in-flight jobs. That idempotency
// is exactly why `citadel init`'s worker-setup hook uses this on an existing
// unit rather than Install (which rewrites the unit file + daemon-reloads).
// System units require root, matching every other systemd.go mutation.
func (u ManagedUnit) EnableNow() error {
	if !u.UserMode && os.Geteuid() != 0 {
		return fmt.Errorf("enabling %s requires root; run: %s", u.Name, u.EnableNowCommand())
	}
	ctl := systemctlArgs(u.UserMode)
	return runCmd("systemctl", append(ctl, "enable", "--now", u.Name)...)
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

// InstalledManagedUnit reports the first citadel-managed systemd unit that
// EXISTS ON DISK on this host, whether or not it is currently active -- the
// counterpart to ActiveManagedUnit, which additionally requires the unit to be
// running.
//
// `citadel init`'s Linux worker-setup hook (citadel-cli#1080) uses this to
// choose between `enable --now`-ing a unit that is already installed
// (install.sh/packer's citadel-worker.service, or a prior `citadel service
// install`) and installing a fresh one. Enabling an existing unit must never
// rewrite its file, so the two cases need to be distinguished BEFORE any
// mutation -- which the active-only ActiveManagedUnit cannot do for a unit that
// is installed but stopped.
//
// Same enumeration + ownership check as ActiveManagedUnit (candidateManagedUnits
// + isCitadelManagedUnit), minus the is-active probe.
func InstalledManagedUnit() (ManagedUnit, bool) {
	for _, cand := range candidateManagedUnits() {
		data, err := os.ReadFile(cand.path)
		if err != nil {
			continue // not present on this host
		}
		if !isCitadelManagedUnit(string(data)) {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(cand.path), ".service")
		return ManagedUnit{Name: name, UserMode: cand.userMode}, true
	}
	return ManagedUnit{}, false
}
