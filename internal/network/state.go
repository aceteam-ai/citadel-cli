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
)

// GetStateDir returns the state directory path for network state.
// This is where tsnet stores WireGuard keys and connection state.
//
// The state directory is determined by:
// 1. First checking the global config file for node_config_dir setting
// 2. If not found, falling back to user home directory
//
// This ensures consistency when running with sudo - state follows the config.
func GetStateDir() string {
	// First, try to get state dir from global config (follows config location)
	if nodeDir := getNodeConfigDirFromGlobalConfig(); nodeDir != "" {
		// On Windows, reject paths under SYSTEM profile — these are written by a
		// service running as LocalSystem and are inaccessible to interactive users.
		if runtime.GOOS == "windows" && isSystemProfilePath(nodeDir) {
			// Fall through to user home directory
		} else {
			return filepath.Join(nodeDir, "network")
		}
	}

	// Fallback to user home directory
	return filepath.Join(getUserHomeDir(), "citadel-node", "network")
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
// On Windows, checks both ProgramData (system-wide, written by services) and
// LOCALAPPDATA (user-local, written by interactive init) locations.
func getNodeConfigDirFromGlobalConfig() string {
	candidates := []string{
		filepath.Join(getGlobalConfigDirForState(), "config.yaml"),
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
