// internal/network/state.go
// State directory management for tsnet network connections
package network

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// machineStatePointerFile is the basename of the world-readable pointer file
// that records the canonical node_config_dir for THIS machine, independent of
// which user/HOME/sudo context invokes citadel. It lives alongside the global
// config under the machine-wide config dir (e.g. /etc/citadel/state-dir on Linux)
// and contains only a filesystem path — never a secret — so it can be readable
// by every user on the box (mode 0644). config.yaml stays 0600 because it holds
// the device API token.
const machineStatePointerFile = "state-dir"

// GetStateDir returns the state directory path for network state.
// This is where tsnet stores its WireGuard/machine key and connection state.
//
// CRITICAL: this path MUST resolve to the same directory on a given machine
// regardless of which user, HOME, or sudo context invokes citadel. If it
// diverges, tsnet opens a different (empty) state dir, mints a fresh machine
// key, and Headscale registers a DUPLICATE node instead of reattaching to the
// existing one (see aceteam-ai/citadel-cli#383). Every branch below therefore
// resolves an owner-consistent path and never trusts the raw invoker $HOME,
// which is /root under sudo.
//
// Resolution order (first match wins):
//  1. Machine-global pointer file (world-readable, written at init) — converges
//     ALL contexts (root service, sudo interactive, any user) on one path.
//  2. node_config_dir from the global config.yaml (SUDO_USER-aware locations).
//  3. Existing tsnet state already present at the legacy owner-home path — reuse
//     it so an already-registered node's machine key is never orphaned on upgrade.
//  4. Fresh fallback: the owner-consistent home path (SUDO_USER-resolved home,
//     NOT the invoker's $HOME).
func GetStateDir() string {
	return resolveStateDir(stateDirInputs{
		pointerDir:        readMachineStatePointer(),
		configNodeDir:     getNodeConfigDirFromGlobalConfig(),
		ownerHome:         getOwnerHomeDir(),
		legacyStateExists: legacyStateExistsAt,
		goos:              runtime.GOOS,
	})
}

// stateDirInputs bundles the (already-resolved) inputs to resolveStateDir so the
// resolution logic is a pure function testable without touching a real HOME,
// config file, or filesystem — mirroring resolveConfigDir / resolveChownTargetWith.
type stateDirInputs struct {
	// pointerDir is the node_config_dir read from the machine-global pointer
	// file, or "" if the file is absent/unreadable.
	pointerDir string
	// configNodeDir is node_config_dir read from the global config.yaml, or "".
	configNodeDir string
	// ownerHome is the owning user's home dir, resolved via SUDO_USER (falls back
	// to the current user), NOT the raw invoker $HOME.
	ownerHome string
	// legacyStateExists reports whether a non-empty tsnet state already lives at
	// <home>/citadel-node/network. Injected so tests avoid the real filesystem.
	legacyStateExists func(home string) bool
	// goos is runtime.GOOS, injected so the Windows SYSTEM-profile guard is testable.
	goos string
}

// resolveStateDir is the pure core of GetStateDir. See GetStateDir for the
// resolution order and the duplicate-node rationale.
func resolveStateDir(in stateDirInputs) string {
	// (1) Machine-global pointer: the only source that converges every user on
	// this box (root-run service vs. sudo interactive vs. a distinct service user).
	if dir := usableNodeDir(in.pointerDir, in.goos); dir != "" {
		return filepath.Join(dir, "network")
	}

	// (2) node_config_dir from the global config file(s).
	if dir := usableNodeDir(in.configNodeDir, in.goos); dir != "" {
		return filepath.Join(dir, "network")
	}

	legacyDir := filepath.Join(in.ownerHome, "citadel-node", "network")

	// (3) Reuse an already-populated legacy state dir so an existing machine key
	// is never abandoned (which would force a re-registration → duplicate node).
	if in.ownerHome != "" && in.legacyStateExists != nil && in.legacyStateExists(in.ownerHome) {
		return legacyDir
	}

	// (4) Fresh machine: owner-consistent home path.
	return legacyDir
}

// usableNodeDir returns the joinable node_config_dir, or "" if it should be
// skipped. On Windows it rejects SYSTEM-profile paths, which are written by a
// service running as LocalSystem and are inaccessible to interactive users.
func usableNodeDir(nodeDir, goos string) string {
	if nodeDir == "" {
		return ""
	}
	if goos == "windows" && isSystemProfilePath(nodeDir) {
		return ""
	}
	return nodeDir
}

// getOwnerHomeDir resolves the home directory of the user that OWNS this node,
// using the SUDO_USER-aware platform helpers rather than the raw invoker $HOME.
// Under `sudo citadel login`, the raw $HOME is /root, but the owning user (and
// where the user-mode service's state actually lives) is $SUDO_USER's home —
// resolving that consistently is the crux of the duplicate-node fix.
func getOwnerHomeDir() string {
	if runtime.GOOS == "windows" {
		return getUserHomeDir()
	}
	if owner := platform.GetSudoUser(); owner != "" {
		if home, err := platform.HomeDir(owner); err == nil && home != "" {
			return home
		}
	}
	return getUserHomeDir()
}

// legacyStateExistsAt reports whether a non-empty tsnet state directory already
// exists at <home>/citadel-node/network. "Non-empty" (not merely present) is
// required so a stale empty network/ dir never wins over a real machine key
// that lives elsewhere.
func legacyStateExistsAt(home string) bool {
	if home == "" {
		return false
	}
	return dirHasEntries(filepath.Join(home, "citadel-node", "network"))
}

// dirHasEntries reports whether path is a directory containing at least one entry.
func dirHasEntries(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// machineStatePointerPath returns the path of the machine-global pointer file.
// Always the fixed machine-wide config dir (never a user-local one) so every
// user on the box reads the same value.
func machineStatePointerPath() string {
	return filepath.Join(getGlobalConfigDirForState(), machineStatePointerFile)
}

// readMachineStatePointer reads node_config_dir from the machine-global pointer
// file, or returns "" if it is absent/unreadable. The file contains only a
// path (optionally surrounded by whitespace).
func readMachineStatePointer() string {
	data, err := os.ReadFile(machineStatePointerPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// WriteMachineStatePointer records nodeConfigDir in the machine-global,
// world-readable pointer file so all invocation contexts on this machine
// converge on the same tsnet state dir. Best-effort: writing under
// getGlobalConfigDirForState() (e.g. /etc/citadel) typically requires root, so
// a non-root init simply skips it and relies on owner-home resolution (which is
// already consistent for the single-user case). Mode 0644 is safe because the
// content is a directory path, not a secret.
func WriteMachineStatePointer(nodeConfigDir string) error {
	if nodeConfigDir == "" {
		return nil
	}
	dir := getGlobalConfigDirForState()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, machineStatePointerFile)
	return os.WriteFile(path, []byte(nodeConfigDir+"\n"), 0644)
}

// isSystemProfilePath returns true if the path is under the Windows SYSTEM
// profile directory, which happens when a service running as LocalSystem
// writes config with its own home directory.
func isSystemProfilePath(p string) bool {
	return strings.Contains(strings.ToLower(p), `\systemprofile`)
}

// getUserHomeDir returns the user's home directory.
// On Windows, uses LOCALAPPDATA/APPDATA to avoid resolving to
// C:\Windows\System32\config\systemprofile when running as SYSTEM.
func getUserHomeDir() string {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return v
		}
		if v := os.Getenv("APPDATA"); v != "" {
			return v
		}
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return os.Getenv("USERPROFILE")
	}

	baseDir, err := os.UserHomeDir()
	if err != nil {
		baseDir = os.Getenv("HOME")
	}
	return baseDir
}

// getNodeConfigDirFromGlobalConfig reads node_config_dir from global config.
// It checks every location where `citadel init` may have written the config:
//   - the machine-wide dir (e.g. /etc/citadel — written by root/sudo init),
//   - the SUDO_USER-aware user-local dir (~/.citadel-cli — written by a non-root
//     init, or by `sudo citadel init` on behalf of $SUDO_USER),
//   - on Windows, LOCALAPPDATA\Citadel.
//
// Reading the user-local location matters because platform.ConfigDir() (where
// init writes) resolves to ~/.citadel-cli when non-root, which the machine-wide
// /etc/citadel read alone would miss — a source of the state-dir divergence.
func getNodeConfigDirFromGlobalConfig() string {
	candidates := []string{
		filepath.Join(getGlobalConfigDirForState(), "config.yaml"),
	}
	// User-local config written by non-root init. Resolve the owner (SUDO_USER-
	// aware) rather than trusting the raw invoker $HOME, which is /root under sudo.
	for _, home := range candidateOwnerHomes() {
		if home == "" {
			continue
		}
		candidates = append(candidates, filepath.Join(home, ".citadel-cli", "config.yaml"))
	}
	// On Windows, platform.ConfigDir() writes to LOCALAPPDATA\Citadel — also check there.
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			candidates = append(candidates, filepath.Join(v, "Citadel", "config.yaml"))
		}
	}

	for _, path := range candidates {
		if dir := readNodeConfigDir(path); dir != "" {
			return dir
		}
	}
	return ""
}

// candidateOwnerHomes returns the home directories that may hold a user-local
// config.yaml: the SUDO_USER-resolved owner home first (the box's owner under
// sudo), then the raw current-user home. Deduplicated, empty entries kept out
// by the caller.
func candidateOwnerHomes() []string {
	homes := []string{}
	if runtime.GOOS != "windows" {
		if owner := platform.GetSudoUser(); owner != "" {
			if home, err := platform.HomeDir(owner); err == nil {
				homes = append(homes, home)
			}
		}
	}
	if h := getUserHomeDir(); h != "" {
		homes = append(homes, h)
	}
	return homes
}

// getGlobalConfigDirForState returns the global config directory path
func getGlobalConfigDirForState() string {
	switch runtime.GOOS {
	case "darwin":
		return "/usr/local/etc/citadel"
	case "windows":
		return filepath.Join(os.Getenv("ProgramData"), "Citadel")
	default:
		return "/etc/citadel"
	}
}

// EnsureStateDir creates the state directory if it doesn't exist.
// If running as root, fixes ownership so the intended non-root user
// (detected via $SUDO_USER or defaulting to "citadel") can access the state.
// Returns the state directory path.
func EnsureStateDir() (string, error) {
	stateDir := GetStateDir()
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return "", err
	}
	// Fix ownership of the state directory and parent node config dir
	// when running as root (e.g. sudo login, systemd pairing).
	fixStateDirOwnership(stateDir)
	return stateDir, nil
}

// ClearState removes all network state, effectively logging out.
// This removes WireGuard keys and forces re-authentication on next connect.
func ClearState() error {
	stateDir := GetStateDir()
	return os.RemoveAll(stateDir)
}

// HasState returns true if network state exists (previously connected).
func HasState() bool {
	stateDir := GetStateDir()
	info, err := os.Stat(stateDir)
	if err != nil {
		return false
	}
	if !info.IsDir() {
		return false
	}

	// Check for tsnet state file
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// GetStatePath returns the full path for a state file.
func GetStatePath(filename string) string {
	return filepath.Join(GetStateDir(), filename)
}

// FixStatePermissions fixes ownership of the state directory and all contents
// so the intended non-root user can access tsnet state files.
// This should be called after tsnet has finished writing state (e.g. after
// Connect succeeds or after Disconnect). No-op if not running as root.
func FixStatePermissions() {
	fixStateDirOwnership(GetStateDir())
}

// resolveChownTarget determines the UID/GID to chown state files to.
// Returns (uid, gid, true) if running as root and a target user is found,
// or (0, 0, false) if not running as root or lookup fails.
func resolveChownTarget() (uid int, gid int, ok bool) {
	return resolveChownTargetWith(os.Getuid(), os.Getenv("SUDO_USER"), user.Lookup)
}

// resolveChownTargetWith is the testable core of resolveChownTarget.
// It accepts injected values instead of reading from the OS directly.
func resolveChownTargetWith(currentUID int, sudoUser string, lookupFn func(string) (*user.User, error)) (uid int, gid int, ok bool) {
	if currentUID != 0 {
		return 0, 0, false // Not root, nothing to fix
	}

	targetUser := sudoUser
	if targetUser == "" {
		targetUser = "citadel" // Default Citadel OS user
	}

	u, err := lookupFn(targetUser)
	if err != nil {
		return 0, 0, false // User not found, can't fix
	}

	parsedUID, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, false
	}
	parsedGID, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, false
	}

	return parsedUID, parsedGID, true
}

// fixStateDirOwnership chowns the state directory, its parent node config dir,
// and all contents to the target non-root user. No-op if not running as root
// or the target user cannot be resolved.
//
// We chown from the parent (node_config_dir) down, not just the network/ dir,
// because the parent may also have been created by root and the non-root user
// needs traverse permission (the dir is mode 0700).
func fixStateDirOwnership(stateDir string) {
	uid, gid, ok := resolveChownTarget()
	if !ok {
		return
	}

	// stateDir is typically <node_config_dir>/network/
	// Chown from the parent (node_config_dir) to cover traverse permissions.
	parentDir := filepath.Dir(stateDir)

	// Safety: only chown if parentDir looks like a citadel node dir,
	// not a system path like /home or /etc.
	parentBase := filepath.Base(parentDir)
	if parentBase != "citadel-node" && parentBase != filepath.Base(getNodeConfigDirFromGlobalConfig()) {
		// If we can't verify the parent is a citadel dir, chown only stateDir
		chownRecursive(stateDir, uid, gid)
		return
	}

	chownRecursive(parentDir, uid, gid)
}

// chownRecursive changes ownership of a directory and all its contents.
// Errors are logged to stderr but do not cause failure — this is best-effort.
func chownRecursive(root string, uid, gid int) {
	if err := os.Chown(root, uid, gid); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not chown %s: %v\n", root, err)
	}
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip inaccessible entries
		}
		if chownErr := os.Chown(path, uid, gid); chownErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not chown %s: %v\n", path, chownErr)
		}
		return nil
	})
}
