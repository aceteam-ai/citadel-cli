// Package hpcscope installs an hpc.Layout as this process's path scope. It is
// separate from internal/hpc so that package stays a leaf, and separate from
// cmd so its process-level effects can be tested in a child process that has
// none of cmd's test fixtures installed.
package hpcscope

import (
	"os"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/hpc"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// Install makes the layout authoritative for the rest of the process: every
// config, home, node-config, network-state and machine-pointer lookup resolves
// inside the job root, and the identity store never adopts a legacy key from
// the shared home. It is one-shot; a second Install with a different layout
// fails. Call it before anything resolves a path.
func Install(layout hpc.Layout) error {
	if err := platform.ScopeProcessDirectories(layout.ConfigDir, layout.HomeDir); err != nil {
		return err
	}
	if err := network.ScopeProcessStateDirectories(layout.NodeDir, layout.MachineDir); err != nil {
		return err
	}
	nodeidentity.DisableLegacyReadThrough()
	// Code that reads HOME or XDG directly (rather than through platform)
	// must also land in the job root.
	for key, value := range Environment(layout) {
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return nil
}

// Environment is the HOME/XDG environment Install sets for the layout.
func Environment(layout hpc.Layout) map[string]string {
	return map[string]string{
		"HOME":            layout.HomeDir,
		"XDG_CONFIG_HOME": filepath.Join(layout.HomeDir, ".config"),
		"XDG_CACHE_HOME":  filepath.Join(layout.HomeDir, ".cache"),
		"XDG_DATA_HOME":   filepath.Join(layout.HomeDir, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(layout.HomeDir, ".local", "state"),
	}
}
