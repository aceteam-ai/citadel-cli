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
func ConfigDir() string {
	// If not running as root/admin, use user-local config
	if !IsRoot() {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, ".citadel-cli")
		}
	}

	// System-wide paths for root/admin
	if IsLinux() {
		return "/etc/citadel"
	}
	if IsDarwin() {
		// On macOS, use /usr/local/etc for consistency with Homebrew conventions
		return "/usr/local/etc/citadel"
	}
	// Windows: Use ProgramData for system-wide config
	return `C:\ProgramData\Citadel`
}

// DefaultNodeDir returns the default directory for node configuration
func DefaultNodeDir(username string) (string, error) {
	home, err := HomeDir(username)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "citadel-node"), nil
}
