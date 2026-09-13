// cmd/init_service.go
//
// macOS-only: `citadel init` installs and starts the node worker as a managed
// launchd service so a fresh install leaves a running, self-updating service
// that survives reboot/login (citadel-cli#1043). On Linux the equivalent unit
// is installed by install.sh (fleet) or `citadel service install`; on Windows
// there is no init-driven service install today.
package cmd

import (
	"fmt"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/service"
)

// maybeInstallDarwinNodeService installs and (re)starts the launchd node
// service on macOS. It is a no-op on other platforms and is deliberately
// conservative about WHEN it installs, so a `KeepAlive` service can never be
// left crash-looping:
//
//   - Skips unless the node has device credentials configured
//     (hasDeviceConfigured) -- a `citadel work` with no creds would exit
//     immediately and launchd would relaunch it every ~10s forever.
//   - Skips when the operator chose to skip the network (NetChoiceSkip).
//
// Root vs non-root decides the service form (advisor review, citadel-cli#1043):
//   - non-root (the common no-sudo `citadel init`): a user LaunchAgent
//     (~/Library/LaunchAgents, domain gui/<uid>) -- starts at LOGIN.
//   - root (e.g. `sudo citadel init --provision`): a system LaunchDaemon
//     (/Library/LaunchDaemons, domain system) -- starts at BOOT without login.
//
// A user LaunchAgent's "survives a reboot" means "re-registers and starts at
// the next login"; true boot-time-without-login requires the daemon form.
func maybeInstallDarwinNodeService(choice nexus.NetworkChoice) {
	if !platform.IsDarwin() {
		return
	}
	if choice == nexus.NetChoiceSkip {
		return
	}
	if !hasDeviceConfigured() {
		// No worker credentials: installing a KeepAlive `citadel work` here
		// would crash-loop. Leave the service uninstalled; the operator can run
		// `citadel login` then `citadel service install` once configured.
		Debug("darwin: skipping launchd service install (no device credentials configured)")
		return
	}

	cfg, err := service.DefaultConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Could not resolve service config for launchd install: %v\n", err)
		return
	}
	cfg.Args = []string{"work"}
	// Root -> system LaunchDaemon (boot-time); non-root -> user LaunchAgent
	// (login-time). Never a gui/0 LaunchAgent for root (that GUI domain doesn't
	// exist).
	cfg.UserMode = !platform.IsRoot()

	mgr := service.NewManager()
	if err := mgr.Install(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Could not install the launchd node service: %v\n", err)
		fmt.Fprintln(os.Stderr, "   Install it manually with: citadel service install")
		return
	}
	if cfg.UserMode {
		fmt.Println("\n✅ Installed the Citadel launchd service (starts at login, survives reboot).")
	} else {
		fmt.Println("\n✅ Installed the Citadel launchd daemon (starts at boot, survives reboot).")
	}
}
