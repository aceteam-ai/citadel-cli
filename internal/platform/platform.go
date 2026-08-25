package platform

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// OS returns the current operating system (linux or darwin)
func OS() string {
	return runtime.GOOS
}

// IsLinux returns true if running on Linux
func IsLinux() bool {
	return runtime.GOOS == "linux"
}

// IsDarwin returns true if running on macOS
func IsDarwin() bool {
	return runtime.GOOS == "darwin"
}

// IsWindows returns true if running on Windows
func IsWindows() bool {
	return runtime.GOOS == "windows"
}

// IsRoot checks if the current user has root/admin privileges
func IsRoot() bool {
	if IsLinux() || IsDarwin() {
		return os.Geteuid() == 0
	}
	if IsWindows() {
		return isWindowsAdmin()
	}
	return false
}

// HomeDir returns the home directory for the given username.
// If username is empty, returns the current user's home directory.
func HomeDir(username string) (string, error) {
	if username == "" {
		return os.UserHomeDir()
	}

	u, err := user.Lookup(username)
	if err != nil {
		return "", fmt.Errorf("failed to lookup user %s: %w", username, err)
	}
	return u.HomeDir, nil
}

// GetSudoUser returns the original username when running with sudo
func GetSudoUser() string {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		return sudoUser
	}

	// Fallback to current user
	if u, err := user.Current(); err == nil {
		return u.Username
	}

	return ""
}

// GetUID returns the UID for the given username
// On Windows, this is not applicable and returns empty string
func GetUID(username string) (string, error) {
	if IsWindows() {
		return "", nil // Windows uses SIDs, not UIDs
	}
	cmd := exec.Command("id", "-u", username)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get UID for %s: %w", username, err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetGID returns the GID for the given username
// On Windows, this is not applicable and returns empty string
func GetGID(username string) (string, error) {
	if IsWindows() {
		return "", nil // Windows uses SIDs, not GIDs
	}
	cmd := exec.Command("id", "-g", username)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get GID for %s: %w", username, err)
	}
	return strings.TrimSpace(string(output)), nil
}

// ChownR changes ownership of a file or directory recursively
// On Windows, this is a no-op as Windows uses ACLs instead of Unix permissions
func ChownR(path, owner string) error {
	if IsWindows() {
		return nil // Windows uses ACLs, not chown
	}

	uid, err := GetUID(owner)
	if err != nil {
		return err
	}

	gid, err := GetGID(owner)
	if err != nil {
		return err
	}

	cmd := exec.Command("chown", "-R", fmt.Sprintf("%s:%s", uid, gid), path)
	return cmd.Run()
}

// ConfigDir returns the appropriate config directory for the OS.
// Uses system-wide paths when running as root, user-local paths otherwise.
// On Windows, always uses a user-local path (%LOCALAPPDATA%\Citadel) to
// match the install location and avoid requiring admin privileges.
//
// When running as root (e.g. via sudo), the invoking user's ~/.citadel-cli
// is preferred over /etc/citadel so that configs written by `citadel init`
// as the normal user are found by `sudo citadel work`.
func ConfigDir() string {
	return resolveConfigDir(IsRoot(), IsWindows(), IsLinux(), IsDarwin())
}

// resolveConfigDir is the testable core of ConfigDir. Parameters are passed
// in so unit tests can exercise the root/sudo code paths without privileges.
func resolveConfigDir(isRoot, isWindows, isLinux, isDarwin bool) string {
	// Windows: always use user-local path regardless of elevation.
	if isWindows {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "Citadel")
		}
		if v := os.Getenv("APPDATA"); v != "" {
			return filepath.Join(v, "Citadel")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "AppData", "Local", "Citadel")
		}
		return `C:\ProgramData\Citadel`
	}

	// Linux/macOS: use user-local config when not root
	if !isRoot {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, ".citadel-cli")
		}
	}

	// Running as root — check whether an explicitly-set HOME points at a
	// user config directory. This covers `sudo -E HOME=/home/citadel ...`
	// where SUDO_USER may be "root" or unset (e.g. systemd launching sudo).
	if home := os.Getenv("HOME"); home != "" {
		candidate := filepath.Join(home, ".citadel-cli")
		if _, err := os.Stat(filepath.Join(candidate, "config.yaml")); err == nil {
			return candidate
		}
	}

	// Fallback: resolve the invoking user's home via SUDO_USER. This handles
	// the case where HOME was not forwarded but sudo preserved SUDO_USER.
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && sudoUser != "root" {
		if home, err := HomeDir(sudoUser); err == nil {
			candidate := filepath.Join(home, ".citadel-cli")
			if _, err := os.Stat(filepath.Join(candidate, "config.yaml")); err == nil {
				return candidate
			}
		}
	}

	// System-wide paths for root/admin
	if isLinux {
		return "/etc/citadel"
	}
	if isDarwin {
		// On macOS, use /usr/local/etc for consistency with Homebrew conventions
		return "/usr/local/etc/citadel"
	}
	return "/etc/citadel"
}

// ConfigDirCandidates returns every directory ConfigDir() could plausibly
// resolve to for SOME invocation context on this machine: a non-root
// process, root/sudo with HOME forwarded, root/sudo resolved via SUDO_USER,
// and a bare root/systemd invocation with no user context at all (the
// shipped citadel-worker.service pins HOME=/root, which resolveConfigDir
// cannot map back to a user's ~/.citadel-cli).
//
// ConfigDir() is invoker-scoped by design (see its doc comment) and returns
// exactly ONE of these paths for the CURRENT process. A caller that needs to
// find state possibly written by a DIFFERENT invocation context on the same
// machine — without paying for the network.GetNodeConfigDir() migration —
// enumerates every candidate instead and checks each one. This is an
// enumeration, not a resolution: it does not decide which candidate is "the"
// config dir, only which directories are worth checking. The current
// process's own ConfigDir() is always first.
func ConfigDirCandidates() []string {
	return configDirCandidates(IsRoot(), IsWindows(), IsLinux(), IsDarwin())
}

// configDirCandidates is the testable core of ConfigDirCandidates, mirroring
// resolveConfigDir's split so the enumeration is exercisable without real
// privileges or a real HOME.
func configDirCandidates(isRoot, isWindows, isLinux, isDarwin bool) []string {
	var out []string
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		out = append(out, d)
	}

	// Always start with what THIS process would resolve to.
	add(resolveConfigDir(isRoot, isWindows, isLinux, isDarwin))

	if isWindows {
		return out // ConfigDir() is invoker-invariant on Windows; nothing else to add.
	}

	// A non-root process's own home (covers e.g. `citadel passcode set` or an
	// APPLY_DEVICE_CONFIG handler run as a normal user).
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".citadel-cli"))
	}
	// Explicit $HOME, if forwarded to a root process (e.g. `sudo -E`).
	if home := os.Getenv("HOME"); home != "" {
		add(filepath.Join(home, ".citadel-cli"))
	}
	// SUDO_USER-derived home (interactive `sudo citadel ...` without -E).
	if su := os.Getenv("SUDO_USER"); su != "" && su != "root" {
		if home, err := HomeDir(su); err == nil {
			add(filepath.Join(home, ".citadel-cli"))
		}
	}
	// System-wide root paths (a systemd worker with HOME=/root and no
	// SUDO_USER at all falls all the way through to these).
	if isDarwin {
		add("/usr/local/etc/citadel")
	} else {
		add("/etc/citadel")
	}
	return out
}

// DefaultNodeDir returns the default directory for node configuration
func DefaultNodeDir(username string) (string, error) {
	home, err := HomeDir(username)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "citadel-node"), nil
}
