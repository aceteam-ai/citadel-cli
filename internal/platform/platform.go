package platform

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
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

// IsRoot checks if the current user has root/admin privileges
func IsRoot() bool {
	if IsLinux() || IsDarwin() {
		return os.Geteuid() == 0
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
func GetUID(username string) (string, error) {
	cmd := exec.Command("id", "-u", username)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get UID for %s: %w", username, err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetGID returns the GID for the given username
func GetGID(username string) (string, error) {
	cmd := exec.Command("id", "-g", username)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get GID for %s: %w", username, err)
	}
	return strings.TrimSpace(string(output)), nil
}

// ChownR changes ownership of a file or directory recursively
func ChownR(path, owner string) error {
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

// ConfigDir returns the appropriate global config directory for the OS
func ConfigDir() string {
	if IsLinux() {
		return "/etc/citadel"
	}
	// On macOS, use /usr/local/etc for consistency with Homebrew conventions
	return "/usr/local/etc/citadel"
}

// DefaultNodeDir returns the default directory for node configuration
func DefaultNodeDir(username string) (string, error) {
	home, err := HomeDir(username)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/citadel-node", home), nil
}
