// Package config provides configuration types for Citadel node settings.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// Permissions controls which capabilities are exposed through the HTTPS gateway
// and the mesh remote-access listeners.
//
// Posture is split by risk (aceteam#6524):
//
//   - Sensitive remote-access surfaces — Console (terminal/shell), Desktop
//     (VNC/screenshot/actions), and Files (filesystem browse/host) — are
//     **default-DENY, opt-in**. A freshly joined node does NOT expose them: it
//     neither advertises them nor serves the corresponding jobs until the
//     operator explicitly enables each one. This flips the previous default-on
//     model, which put a machine's console/screen/files on the org mesh the
//     moment it joined (the White Whale onboarding landmine). Shell (arbitrary
//     code as root) was already default-deny (#6149, Phase 0).
//   - Lower-stakes surfaces — Services, SSH, Provision — keep the default-on
//     (opt-out) model. They mediate node operation the org already owns and do
//     not expose the operator's interactive machine surface the way
//     console/desktop/files do.
//
// Enabling a sensitive surface is NOT the same as opening it to anyone on the
// org mesh: a per-node passcode (PasscodeHash) gates actual access. "Enabled"
// means "reachable IF you also present the node passcode." A capability that is
// enabled but has no passcode set fails CLOSED (access denied) — enablement
// without a passcode never silently opens the surface.
type Permissions struct {
	Console   bool `yaml:"console" json:"console"`     // Terminal WebSocket access (default-deny, opt-in)
	Desktop   bool `yaml:"desktop" json:"desktop"`     // VNC, screenshots, actions (default-deny, opt-in)
	Files     bool `yaml:"files" json:"files"`         // File browser API (default-deny, opt-in)
	Services  bool `yaml:"services" json:"services"`   // Service list/management (default-on, opt-out)
	SSH       bool `yaml:"ssh" json:"ssh"`             // SSH authorized_keys sync (default-on, opt-out)
	Provision bool `yaml:"provision" json:"provision"` // Container provisioning API (default-on, opt-out)
	Shell     bool `yaml:"shell" json:"shell"`         // SHELL_COMMAND job execution (default-deny, opt-in)

	// PasscodeHash is the bcrypt hash of the per-node passcode that gates the
	// sensitive remote-access surfaces (console/desktop/files). It is never the
	// plaintext PIN — bcrypt embeds its own salt, so no separate salt field is
	// stored. Empty means no passcode is set, in which case every sensitive
	// surface fails closed even if its bool is true (see VerifyPasscode).
	PasscodeHash string `yaml:"passcode_hash,omitempty" json:"passcode_hash,omitempty"`
}

const permissionsFile = "permissions.yaml"

// bcryptPasscodeCost is deliberately the library default. The passcode is a
// short interactive PIN, not a password database, and verification runs on the
// node's access path; the default cost keeps that check fast while still salting
// + stretching so a leaked permissions.yaml does not reveal the PIN.
const bcryptPasscodeCost = bcrypt.DefaultCost

// DefaultPermissions returns the default node posture: the sensitive
// remote-access surfaces (Console, Desktop, Files, Shell) are DISABLED, and the
// lower-stakes operational surfaces (Services, SSH, Provision) are enabled.
//
// Default-deny for console/desktop/files is intentional (aceteam#6524): joining
// the fabric to serve a model must never, by itself, put the operator's
// terminal, screen, or filesystem on the org mesh. The operator opts each one in
// explicitly (Control Center or APPLY_DEVICE_CONFIG) and sets a passcode.
func DefaultPermissions() *Permissions {
	return &Permissions{
		Console:   false,
		Desktop:   false,
		Files:     false,
		Services:  true,
		SSH:       true,
		Provision: true,
		Shell:     false,
	}
}

// LoadPermissions reads permissions from the config directory.
// If the file doesn't exist, returns defaults (see DefaultPermissions:
// console/desktop/files/shell disabled, services/ssh/provision enabled).
// Partial files preserve defaults for absent keys (unmarshal into a
// pre-initialized struct), so a config that predates a key keeps its default.
func LoadPermissions(configDir string) *Permissions {
	p := DefaultPermissions()

	data, err := os.ReadFile(filepath.Join(configDir, permissionsFile))
	if err != nil {
		return p
	}

	// yaml.Unmarshal only overwrites keys present in the file, so absent keys
	// keep their default value.
	_ = yaml.Unmarshal(data, p)
	return p
}

// SavePermissions writes permissions to the config directory. The file is
// written 0600 because it now carries the node passcode hash (a credential):
// even though bcrypt makes the hash non-reversible cheaply, there is no reason
// to leave it group/world-readable.
func SavePermissions(configDir string, p *Permissions) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal permissions: %w", err)
	}

	return os.WriteFile(filepath.Join(configDir, permissionsFile), data, 0600)
}

// SetPasscode hashes pin with bcrypt and stores it in PasscodeHash. An empty pin
// clears the passcode (HasPasscode becomes false), which — combined with the
// fail-closed VerifyPasscode — re-locks every sensitive surface. The caller is
// responsible for persisting via SavePermissions.
func (p *Permissions) SetPasscode(pin string) error {
	if strings.TrimSpace(pin) == "" {
		p.PasscodeHash = ""
		return nil
	}
	// Anti-resurrection guard + local-only master PIN (aceteam-ai/citadel-cli#796).
	// Once a master PIN is enrolled, writing a fresh bcrypt hash here would
	// re-create exactly the cheap offline brute-force target enrollment deleted,
	// and would let a platform push (APPLY_DEVICE_CONFIG.NodePasscode) set the
	// gate secret remotely — the master PIN is set LOCALLY only. So refuse any
	// non-empty set while a vault is enrolled; clearing (empty pin) stays
	// allowed (the enrollment path itself uses it to delete the legacy hash).
	if VaultConfigured != nil && VaultConfigured() {
		return fmt.Errorf("a node master PIN is enrolled; it is set locally only — use 'citadel passcode rotate' (the legacy/platform passcode path is disabled)")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pin), bcryptPasscodeCost)
	if err != nil {
		return fmt.Errorf("hash passcode: %w", err)
	}
	p.PasscodeHash = string(hash)
	return nil
}

// Master-PIN delegation hooks (aceteam-ai/citadel-cli#796).
//
// When a node migrates to the master PIN, the legacy bcrypt PasscodeHash is
// DELETED (a bcrypt hash cannot yield an encryption key, and leaving it behind
// re-introduces the cheap brute-force target the KDF exists to remove). If the
// gate readers still only consulted PasscodeHash, every gate would fail closed
// the moment a node enrolled — bricking remote access.
//
// The master vault lives under network.GetNodeConfigDir(), and this package is
// a widely-imported leaf that must NOT import network (which drags in the whole
// tailscale stack). So the gate is made vault-aware through these package-level
// hooks, wired once at startup in package cmd (which already imports network +
// nodevault). Every existing gate call site keeps calling VerifyPasscode /
// HasPasscode unchanged and transparently gets the master-PIN answer.
//
// Contract:
//   - VaultConfigured reports whether a master PIN is enrolled on this node.
//   - VaultVerify checks a PIN against the vault and returns (ok, handled).
//     handled==false means "no vault; use the legacy bcrypt path" so un-migrated
//     nodes behave exactly as before.
//   - When the hooks are nil (e.g. a binary that never wired them, or a unit
//     test), behavior falls back to the legacy bcrypt path — never a panic.
var (
	VaultConfigured func() bool
	VaultVerify     func(pin string) (ok bool, handled bool)
)

// HasPasscode reports whether a node passcode (legacy) OR a master PIN is set.
func (p *Permissions) HasPasscode() bool {
	if VaultConfigured != nil && VaultConfigured() {
		return true
	}
	return p.PasscodeHash != ""
}

// VerifyPasscode reports whether pin matches the node's access secret. When a
// master PIN is enrolled it delegates to the vault (rate-limited, lockout-gated,
// verify-by-unwrap); otherwise it verifies against the legacy bcrypt hash. It
// fails CLOSED: an unset secret or an empty pin returns false, so a sensitive
// surface that was enabled but never given a passcode stays locked rather than
// silently opening to anyone on the org mesh.
func (p *Permissions) VerifyPasscode(pin string) bool {
	if VaultVerify != nil {
		if ok, handled := VaultVerify(pin); handled {
			return ok
		}
	}
	if p.PasscodeHash == "" || pin == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(p.PasscodeHash), []byte(pin)) == nil
}

// IsSensitiveCategory reports whether a permission category is a passcode-gated
// sensitive remote-access surface. Kept as a package function so the gateway and
// listener paths agree on the set without duplicating the string literals.
//
// "shell" belongs here (citadel#763): internal/jobs/shell_command.go gates an
// enabled Shell handler on VerifyPasscode exactly like console/desktop/files,
// so this switch — the package's own stated authority for "what is
// sensitive" — must not disagree with that enforcement.
func IsSensitiveCategory(category string) bool {
	switch category {
	case "console", "desktop", "files", "shell":
		return true
	default:
		return false
	}
}
