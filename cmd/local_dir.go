package cmd

import (
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// localStateDirName is the subdirectory of the invoking user's config dir that
// holds local co-browse state: the owner lock, the audit log and the viewer
// token.
const localStateDirName = "local"

// localStateDir is where local co-browse keeps its state. Local mode needs no
// AceTeam account and has one owner per OS user, so this is the invoking
// user's config dir (platform.ConfigDir), never the machine-convergent node
// config dir: on a computer with an enrolled node that belongs to the node's
// account, and every other user would be locked out (#1592). Both MCP entry
// points, the viewer, audit and doctor resolve it here so they agree on one
// owner lock per user. A var so tests can point it at a temp dir.
var localStateDir = func() string {
	return filepath.Join(platform.ConfigDir(), localStateDirName)
}
