package cmd

import (
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

// init wires the master-PIN vault into the legacy passcode gate
// (aceteam-ai/citadel-cli#796).
//
// The vault lives under network.GetNodeConfigDir(); internal/config is a leaf
// that must not import network. So config exposes hooks and we bind them here,
// in package cmd, which every citadel entrypoint loads (main → cmd.Execute).
// Once a node enrolls a master PIN, the legacy bcrypt PasscodeHash is deleted;
// these hooks are what keep VerifyPasscode / HasPasscode — and therefore every
// existing gate (terminal, gateway, SHELL_COMMAND, Control Center) plus the
// has_passcode heartbeat — answering correctly without touching a single gate
// call site.
func init() {
	config.VaultConfigured = func() bool {
		return nodevault.Open(network.GetNodeConfigDir()).IsConfigured()
	}
	config.VaultVerify = func(pin string) (ok bool, handled bool) {
		v := nodevault.Open(network.GetNodeConfigDir())
		if !v.IsConfigured() {
			return false, false // no master PIN: fall back to legacy bcrypt
		}
		// Reject an empty/absent passcode for free (fail closed, no KDF, no
		// lockout accounting) so unauthenticated per-connection probes can't
		// lock the node — matching the legacy gate's zero-cost empty reject.
		if pin == "" {
			return false, true
		}
		return v.VerifyPIN(pin) == nil, true
	}
}

// masterVault opens the node's master-PIN vault at the resolved node config
// directory. Shared by the passcode set/rotate/enroll paths.
func masterVault() *nodevault.Vault {
	return nodevault.Open(network.GetNodeConfigDir())
}
