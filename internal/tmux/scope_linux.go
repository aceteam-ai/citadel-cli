//go:build linux

package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
)

const systemdInvocationIDEnv = "INVOCATION_ID"

var (
	lookupSystemdRun = exec.LookPath
	readSelfCgroup   = func() ([]byte, error) { return os.ReadFile("/proc/self/cgroup") }
	currentEUID      = os.Geteuid
	scopeSequence    atomic.Uint64
)

// persistentSessionCommand deliberately handles only the service-manager
// contexts where moving the child is known to work without extra privileges:
// a systemd user service talks to its own user manager, while a root system
// service talks to the system manager. A non-root process in a system unit
// cannot create a sibling system scope, so it keeps the old direct command
// instead of turning a working terminal into a failed systemd-run invocation.
func persistentSessionCommand(sessionName string, command []string) scopeDecision {
	if os.Getenv(systemdInvocationIDEnv) == "" {
		return scopeDecision{command: command, reason: "process is not running in a systemd service (INVOCATION_ID is unset)"}
	}
	systemdRun, err := lookupSystemdRun("systemd-run")
	if err != nil {
		return scopeDecision{command: command, reason: "systemd-run is unavailable"}
	}

	cgroup, err := readSelfCgroup()
	if err != nil {
		return scopeDecision{command: command, reason: fmt.Sprintf("cannot read /proc/self/cgroup: %v", err)}
	}
	userManager := isUserManagerCgroup(string(cgroup))
	if !userManager && currentEUID() != 0 {
		return scopeDecision{command: command, reason: "non-root process in a system service cannot create a sibling system scope"}
	}

	args := make([]string, 0, len(command)+7)
	if userManager {
		args = append(args, "--user")
	}
	args = append(args,
		"--scope",
		"--quiet",
		"--collect",
		"--unit="+nextScopeUnit(sessionName),
		"--",
	)
	args = append(args, command...)
	manager := "system manager"
	if userManager {
		manager = "user manager"
	}
	return scopeDecision{
		command: append([]string{systemdRun}, args...),
		scoped:  true,
		reason:  manager,
	}
}

func isUserManagerCgroup(cgroup string) bool {
	for _, line := range strings.Split(cgroup, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		for _, component := range strings.Split(parts[2], "/") {
			if strings.HasPrefix(component, "user@") && strings.HasSuffix(component, ".service") {
				return true
			}
		}
	}
	return false
}

func nextScopeUnit(sessionName string) string {
	// ValidateSessionName already restricts sessionName to systemd-unit-safe
	// ASCII. The PID plus process-local sequence prevents reconnects and
	// concurrent attaches from colliding with a scope that still owns the tmux
	// server or an attached client.
	n := scopeSequence.Add(1)
	return fmt.Sprintf("citadel-session-%s-%d-%d.scope", sessionName, os.Getpid(), n)
}
