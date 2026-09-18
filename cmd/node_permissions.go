package cmd

import (
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

func saveNodePermissions(p *config.Permissions) error {
	return config.SavePermissions(nodePermissionsDir(), p)
}
