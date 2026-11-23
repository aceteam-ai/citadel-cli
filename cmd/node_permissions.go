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

// saveNodePermissions writes p as the whole policy. It refuses when the file
// on disk cannot be parsed: p was then loaded as the defaults, and saving it
// would silently drop every setting the file holds (including co-browse
// public-only).
func saveNodePermissions(p *config.Permissions) error {
	return savePermissionsChecked(nodePermissionsDir(), p)
}

func savePermissionsChecked(dir string, p *config.Permissions) error {
	if _, err := config.LoadPermissionsStrict(dir); err != nil {
		return fmt.Errorf("the node permissions file must be readable before it can be changed: %w", err)
	}
	return config.SavePermissions(dir, p)
}
