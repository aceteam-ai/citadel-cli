package tmux

// scopeDecision records both the command to execute and why it is (or is not)
// wrapped in a transient service-manager scope. Keeping the reason alongside
// the decision prevents the diagnostic from drifting away from the branch that
// selected it.
type scopeDecision struct {
	command []string
	scoped  bool
	reason  string
}

// PersistentSessionCommand moves a tmux create or attach command into a
// service-manager scope when the platform can do so safely. A tmux server is a
// long-lived child: if Citadel is the process that starts it, leaving it in the
// citadel.service control group makes systemd kill every persistent session on
// a worker restart.
//
// Unsupported platforms and processes not running under a compatible service
// manager receive the original command unchanged. The returned slice is always
// a fresh allocation so callers can safely retain or modify their input.
func PersistentSessionCommand(sessionName string, command []string) []string {
	return persistentSessionDecision(sessionName, command).command
}

func persistentSessionDecision(sessionName string, command []string) scopeDecision {
	copyOf := append([]string(nil), command...)
	if len(copyOf) == 0 {
		return scopeDecision{command: copyOf}
	}
	if err := ValidateSessionName(sessionName); err != nil {
		logf("tmux session %q launch is unscoped: invalid session name", sessionName)
		return scopeDecision{command: copyOf, reason: "invalid session name"}
	}
	decision := persistentSessionCommand(sessionName, copyOf)
	if decision.scoped {
		logf("tmux session %q launch uses a transient systemd scope: %s", sessionName, decision.reason)
	} else {
		logf("tmux session %q launch is unscoped: %s", sessionName, decision.reason)
	}
	return decision
}

func identitySessionCommand(_ string, command []string) scopeDecision {
	return scopeDecision{command: append([]string(nil), command...), reason: "scope disabled"}
}
