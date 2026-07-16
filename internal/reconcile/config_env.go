package reconcile

import (
	"os"
	"strings"
)

// PullEnvVar is an operator KILL SWITCH for the desired-state PULL reconcile
// loop (aceteam#4273). The loop is ON by default — a node with control-plane
// desired-state rows converges automatically. Set CITADEL_RECONCILE_PULL to a
// falsy value ("0", "false", "no", "off") to disable it fleet-wide without a
// binary rollback (emergency brake). Safety against a misconfigured control
// plane wiping every module is provided independently by Reconciler.RefuseFullWipe,
// which no-ops an empty desired state; the kill switch additionally covers
// erroneous installs.
const PullEnvVar = "CITADEL_RECONCILE_PULL"

// PullDisabled reports whether the operator has explicitly turned the pull loop
// OFF via the kill switch. Default (unset or any non-falsy value) is enabled.
// Intended to be read once at startup (the loop is wired or not for the process
// lifetime).
func PullDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(PullEnvVar))) {
	case "0", "false", "no", "off":
		return true
	default:
		return false
	}
}
