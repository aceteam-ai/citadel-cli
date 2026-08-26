package cmd

import (
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// init wires internal/config's DeviceConfigDirsHook (citadel-cli#845) the
// same way cmd/nodevault_hooks.go wires config.VaultConfigured/VaultVerify:
// internal/config is a leaf that must not import internal/network, so the
// directory list is resolved here, in package cmd, which every citadel
// entrypoint loads (main -> cmd.Execute).
func init() {
	config.DeviceConfigDirsHook = deviceConfigDirs
}

// deviceConfigDirs returns, in preference order, the directories that may
// hold the device/org config file (config.yaml: device_api_token,
// api_base_url, org_id, org_name, user_email, user_name, redis_url,
// aceteam_api_key).
//
// network.GetNodeConfigDir() -- the machine-convergent node config dir,
// already used for identity.json/ssh_sync.yaml -- is preferred (citadel-cli
// #845): it resolves to the SAME directory regardless of which user/HOME/sudo
// context invokes citadel, so a root-owned systemd `citadel work` and an
// interactive non-root `citadel whoami`/`status` agree on where this file is.
// platform.ConfigDir() -- invoker-scoped, and where every citadel init/login
// wrote this file before #845 -- is checked second, as a read-only fallback so
// a node registered before this fix isn't stranded until its next citadel
// init/login/reauth writes the config at the new location. See CLAUDE.md's
// ConfigDir()/GetNodeConfigDir() section and citadel-cli#696/#726/#845.
//
// getDeviceConfigFromFile (cmd/work.go) and config.LoadDeviceCredsConverged
// (via the hook above) both resolve through this single function, so cmd/ and
// internal/worker agree on the search order.
func deviceConfigDirs() []string {
	return []string{network.GetNodeConfigDir(), platform.ConfigDir()}
}
