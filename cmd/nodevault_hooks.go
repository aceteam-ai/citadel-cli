package cmd

import (
	"errors"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

// passcodeHookNodeDir resolves the node config directory the passcode gate hooks
// read and write (vault header, lockout state). It is a variable only so this
// package's tests can point the hooks at a temporary directory (see TestMain):
// the real resolver can name a live node's directory on a development machine,
// and a wrong-passcode test would otherwise record failures in its lockout.
var passcodeHookNodeDir = network.GetNodeConfigDir

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
		return nodevault.Open(passcodeHookNodeDir()).IsConfigured()
	}
	config.VaultVerify = func(pin string) (ok bool, handled bool) {
		nodeDir := passcodeHookNodeDir()
		v := nodevault.Open(nodeDir)
		if !v.IsConfigured() {
			return false, false // no master PIN: fall back to legacy bcrypt
		}
		// Reject an empty/absent passcode for free (fail closed, no KDF, no
		// lockout accounting) so unauthenticated per-connection probes can't
		// lock the node — matching the legacy gate's zero-cost empty reject.
		if pin == "" {
			return false, true
		}
		err := v.VerifyPIN(pin)
		warnPasscodeLockoutUnavailableFor(passcodeWarningMaster, nodeDir, err)
		return err == nil, true
	}
	// Legacy bcrypt passcode checks (no master PIN enrolled) get the vault's
	// node-wide persisted lockout policy (#1398), with state in a sibling file,
	// so the HTTP status server, the terminal WebSocket servers and the
	// gateway's sensitive and cache routes are rate-limited in legacy mode too.
	config.LegacyPasscodeLockout = func() config.PasscodeAttemptLimiter {
		return nodevault.OpenLegacyPasscodeLockout(passcodeHookNodeDir())
	}
	config.PasscodeAttemptError = warnPasscodeLockoutUnavailable
}

// The legacy callback contract remains error-only. A healthy attempt does not
// reset the reminder or claim recovery; availability is a separate snapshot.
func warnPasscodeLockoutUnavailable(err error) {
	if errors.Is(err, nodevault.ErrLockoutUnavailable) {
		warnPasscodeLockoutUnavailableFor(passcodeWarningLegacy, passcodeHookNodeDir(), err)
	}
}

// masterVault opens the node's master-PIN vault at the resolved node config
// directory. Shared by the passcode set/rotate/enroll paths.
func masterVault() *nodevault.Vault {
	return nodevault.Open(network.GetNodeConfigDir())
}
