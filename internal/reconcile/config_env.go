package reconcile

import (
	"os"
	"strings"
)

// PullEnvVar gates the desired-state PULL reconcile loop (aceteam#4273). The
// loop is OFF unless this is explicitly set to a truthy value ("1", "true",
// "yes", "on"), mirroring status.AutoStopEnvVar: a node NEVER accepts remote
// desired-state management (which can install/uninstall compose stacks) unless
// the operator has opted in.
const PullEnvVar = "CITADEL_RECONCILE_PULL"

// PullEnabled reports whether the operator opted into the pull loop. Default is
// false; any value other than a recognized truthy token leaves it off. Intended
// to be read once at startup (the loop is wired or not for the process lifetime).
func PullEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(PullEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
