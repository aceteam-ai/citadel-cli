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

// savePermissionsChecked also keeps, from the file as it is now, every field
// the Control Center never edits, so a struct loaded earlier cannot turn them
// back. CobrowsePublicOnly is such a field: only APPLY_DEVICE_CONFIG (on only)
// and 'citadel permission cobrowse-public-only' change it.
func savePermissionsChecked(dir string, p *config.Permissions) error {
	cur, err := config.LoadPermissionsStrict(dir)
	if err != nil {
		return fmt.Errorf("the node permissions file must be readable before it can be changed: %w", err)
	}
	next := *p
	next.CobrowsePublicOnly = cur.CobrowsePublicOnly
	return config.SavePermissions(dir, &next)
}

// toggleNodePermission sets one Built-in Services toggle against the file as
// it is now: a strict load, one field, a save. Nothing else is written back.
func toggleNodePermission(dir, name string, enabled bool) error {
	p, err := config.LoadPermissionsStrict(dir)
	if err != nil {
		return fmt.Errorf("the node permissions file must be readable before it can be changed: %w", err)
	}
	switch name {
	case "Console":
		p.Console = enabled
	case "Desktop":
		p.Desktop = enabled
	case "Files":
		p.Files = enabled
	case "Services":
		p.Services = enabled
	case "SSH":
		p.SSH = enabled
	case "Provision":
		p.Provision = enabled
	case "Shell":
		p.Shell = enabled
	default:
		return fmt.Errorf("unknown permission %q", name)
	}
	return config.SavePermissions(dir, p)
}
