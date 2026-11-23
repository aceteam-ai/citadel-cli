package cmd

import (
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/network"
)

// nodePermissionsDir is the machine-convergent store for the permission policy
// enforced by a Citadel worker. platform.ConfigDir is invoker-scoped, so a
// system service and an interactive control center can otherwise read and write
// different permissions.yaml files for the same enrolled node.
func nodePermissionsDir() string {
	return network.GetNodeConfigDir()
}

func loadNodePermissions() *config.Permissions {
	return config.LoadPermissions(nodePermissionsDir())
}

// saveControlCenterPermissions is the Control Center's save. It loads the
// file strictly first and refuses one it cannot parse, so a save never
// rewrites a damaged file from the defaults. The Control Center holds a copy
// it loaded earlier, which may predate a setup-page connect, so the settings
// it has no toggle for, the setup posture marker and the co-browse
// public-only guard, are taken from the file as it is now (citadel-cli#1867
// C5).
func saveControlCenterPermissions(dir string, p *config.Permissions) error {
	cur, err := config.LoadPermissionsStrict(dir)
	if err != nil {
		return fmt.Errorf("the node permissions file could not be read, so nothing was saved: %w", err)
	}
	out := *p
	out.SetupPosture = cur.SetupPosture
	out.CobrowsePublicOnly = cur.CobrowsePublicOnly
	return config.SavePermissions(dir, &out)
}
