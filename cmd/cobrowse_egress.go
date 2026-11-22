package cmd

import (
	"fmt"
	"log"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func init() {
	// The node-wide co-browse manager reads the public-only setting at every
	// session start (citadel-cli#1867 C4b), in every worker this binary runs
	// (citadel work and the control center). Resolved lazily, never at init.
	platform.SetNodeCobrowseEgressSource(nodeCobrowsePublicOnly)
}

func nodeCobrowsePublicOnly() bool {
	return cobrowsePublicOnlyIn(nodePermissionsDir())
}

// cobrowsePublicOnlyIn reads the setting from permissionsDir. A file that
// exists but cannot be read or parsed counts as public-only: the lenient
// loader would read it as the defaults, which here means no guard.
func cobrowsePublicOnlyIn(permissionsDir string) bool {
	p, err := config.LoadPermissionsStrict(permissionsDir)
	if err != nil {
		log.Printf("[cobrowse] node permissions unreadable, remote sessions run public-only: %v", err)
		return true
	}
	return p.CobrowsePublicOnly
}

// setCobrowsePublicOnly is the local command's pure core. It refuses a
// permissions file it cannot parse rather than rewrite it from defaults.
func setCobrowsePublicOnly(permissionsDir string, enabled bool) error {
	p, err := config.LoadPermissionsStrict(permissionsDir)
	if err != nil {
		return fmt.Errorf("the node permissions file must be readable before this setting can change: %w", err)
	}
	p.CobrowsePublicOnly = enabled
	if err := config.SavePermissions(permissionsDir, p); err != nil {
		return fmt.Errorf("save co-browse public-only setting: %w", err)
	}
	return nil
}
