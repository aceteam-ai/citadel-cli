// Package config provides configuration types for Citadel node settings.
package config

import (
	"errors"
	"fmt"
	"io/fs"
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
	// keep their default value. Discard the partially decoded value on any parse
	// error: a malformed file that starts with "shell: true" must fail closed.
	if err := yaml.Unmarshal(data, p); err != nil {
		return DefaultPermissions()
	}
	return p
}

// LoadPermissionsStrict is LoadPermissions for a gate decision: a missing file
// still yields the defaults, but any other read error, or a file that does not
// parse, is returned instead of being read as the defaults. A gate whose
// outcome depends on a configured passcode must refuse on that error rather
// than conclude that no passcode is set.
func LoadPermissionsStrict(configDir string) (*Permissions, error) {
	p := DefaultPermissions()
	data, err := os.ReadFile(filepath.Join(configDir, permissionsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read permissions: %w", err)
	}
	if err := yaml.Unmarshal(data, p); err != nil {
		return nil, fmt.Errorf("parse permissions: %w", err)
	}
	return p, nil
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

	// Write a temp file and rename it over the old one, so a concurrent reader
	// sees either the old or the new file, never a partly written one.
	tmp, err := os.CreateTemp(configDir, permissionsFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("create permissions temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod permissions temp file: %w", err)
	}
	if err := keepOwner(tmp, filepath.Join(configDir, permissionsFile)); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close permissions temp file: %w", err)
	}
	return os.Rename(tmpName, filepath.Join(configDir, permissionsFile))
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
	return p.VerifyLegacyPasscode(pin)
}

// ErrPasscodeMismatch is CheckLegacyPasscode's result for an unset hash, an
// empty pin, or (with no limiter wired) a wrong pin.
var ErrPasscodeMismatch = errors.New("config: passcode does not match")

// PasscodeAttemptLimiter is the node-wide persisted attempt limiter applied to
// legacy bcrypt passcode checks (#1398). Attempt runs check unless a lockout is
// active, records the result, and returns nil only for a successful check. It is
// satisfied by *nodevault.Lockout, which applies the master-PIN vault's policy.
type PasscodeAttemptLimiter interface {
	Attempt(check func() bool) error
}

// LegacyPasscodeLockout returns the limiter for legacy passcode checks. Like
// VaultVerify it is a hook wired in package cmd, because the limiter's state
// lives under network.GetNodeConfigDir() and this leaf package must not import
// network. When nil (a unit test, or a binary that never wired it), legacy
// checks are unlimited, as before #1398.
var LegacyPasscodeLockout func() PasscodeAttemptLimiter

// VerifyLegacyPasscode checks pin against the legacy bcrypt passcode hash only,
// never the master-PIN hooks. It fails closed on an unset hash or empty pin, and
// is subject to the node-wide lockout when LegacyPasscodeLockout is wired.
func (p *Permissions) VerifyLegacyPasscode(pin string) bool {
	var lim PasscodeAttemptLimiter
	if LegacyPasscodeLockout != nil {
		lim = LegacyPasscodeLockout()
	}
	return p.CheckLegacyPasscode(pin, lim) == nil
}

// CheckLegacyPasscode is VerifyLegacyPasscode with an explicit limiter and the
// reason preserved: nil on a match, ErrPasscodeMismatch for an unset hash or an
// empty pin (rejected before the limiter, so neither consumes a failure), and
// otherwise whatever lim.Attempt returns (for *nodevault.Lockout: ErrWrongPIN on
// a mismatch, *ErrLockedOut while locked out). A nil lim checks the hash
// directly with no accounting.
func (p *Permissions) CheckLegacyPasscode(pin string, lim PasscodeAttemptLimiter) error {
	if p.PasscodeHash == "" || pin == "" {
		return ErrPasscodeMismatch
	}
	hash := p.PasscodeHash
	compare := func() bool {
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pin)) == nil
	}
	if lim == nil {
		if compare() {
			return nil
		}
		return ErrPasscodeMismatch
	}
	return lim.Attempt(compare)
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
