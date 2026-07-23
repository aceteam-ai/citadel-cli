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
	hash, err := bcrypt.GenerateFromPassword([]byte(pin), bcryptPasscodeCost)
	if err != nil {
		return fmt.Errorf("hash passcode: %w", err)
	}
	p.PasscodeHash = string(hash)
	return nil
}

// HasPasscode reports whether a node passcode is set.
func (p *Permissions) HasPasscode() bool {
	return p.PasscodeHash != ""
}

// VerifyPasscode reports whether pin matches the stored node passcode. It fails
// CLOSED: an unset passcode (no hash) or an empty pin returns false, so a
// sensitive surface that was enabled but never given a passcode stays locked
// rather than silently opening to anyone on the org mesh.
func (p *Permissions) VerifyPasscode(pin string) bool {
	if p.PasscodeHash == "" || pin == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(p.PasscodeHash), []byte(pin)) == nil
}

// IsSensitiveCategory reports whether a permission category is a passcode-gated
// sensitive remote-access surface. Kept as a package function so the gateway and
// listener paths agree on the set without duplicating the string literals.
func IsSensitiveCategory(category string) bool {
	switch category {
	case "console", "desktop", "files":
		return true
	default:
		return false
	}
}
