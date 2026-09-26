// Package service provides platform-agnostic managed service installation
// for the Citadel node agent (systemd, launchd, Windows Service).
package service

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/update"
)

// Manager is the platform-specific interface for managing the Citadel
// system service (install, uninstall, start, stop, status).
type Manager interface {
	Install(config ServiceConfig) error
	Uninstall() error
	Start() error
	Stop() error
	Status() (*ServiceStatus, error)
}

// ServiceConfig holds the parameters needed to install the Citadel service.
type ServiceConfig struct {
	// ExecPath is the absolute path to the citadel binary.
	ExecPath string

	// Args are the arguments passed to the binary (e.g., ["work", "--gateway"]).
	Args []string

	// Description is the human-readable service description.
	Description string

	// UserMode installs as a user service (systemd --user / launchd LaunchAgent)
	// instead of a system-wide service. Ignored on Windows.
	UserMode bool
}

// ServiceStatus describes the current state of the installed service.
type ServiceStatus struct {
	Installed  bool
	Running    bool
	PID        int
	Uptime     time.Duration
	RecentLogs []string // last few lines from the service log
}

// DefaultDescription is the service description used when none is provided.
const DefaultDescription = "Citadel Node Agent - AceTeam Sovereign Compute"

// ServiceName is the identifier used across all platforms.
const ServiceName = "citadel"

// NewManager returns the platform-appropriate Manager. The returned value
// is always non-nil; unsupported platforms get an error only when a method
// is called.
func NewManager() Manager {
	return newPlatformManager()
}

// Validate checks that a ServiceConfig is usable.
func (c *ServiceConfig) Validate() error {
	if c.ExecPath == "" {
		return fmt.Errorf("ExecPath must not be empty")
	}
	if !filepath.IsAbs(c.ExecPath) {
		return fmt.Errorf("ExecPath must be an absolute path, got %q", c.ExecPath)
	}
	if _, err := os.Stat(c.ExecPath); err != nil {
		return fmt.Errorf("ExecPath does not exist: %w", err)
	}
	if c.Description == "" {
		c.Description = DefaultDescription
	}
	return nil
}

// DefaultConfig builds a ServiceConfig from the running binary with
// sensible defaults. The binary path is resolved via os.Executable.
func DefaultConfig() (ServiceConfig, error) {
	exePath, err := os.Executable()
	if err != nil {
		return ServiceConfig{}, fmt.Errorf("failed to get executable path: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return ServiceConfig{}, fmt.Errorf("failed to resolve executable path: %w", err)
	}

	// On macOS, a Homebrew install resolves (via EvalSymlinks) to a VERSIONED
	// Cellar path that `brew upgrade` later removes. Bake the STABLE
	// <prefix>/bin/citadel symlink into the service instead, so a launchd
	// plist keeps pointing at a valid binary across `brew upgrade`
	// (citadel-cli#1043). A non-Homebrew install (curl|bash into ~/.local/bin
	// or /usr/local/bin) is already stable and left unchanged.
	if runtime.GOOS == "darwin" {
		exePath, _ = update.StableDarwinExecPath(exePath)
	}
	args := []string{"work"}
	if runtime.GOOS == "darwin" && bundledDesktopHelper(exePath) {
		if os.Geteuid() == 0 {
			return ServiceConfig{}, fmt.Errorf("desktop helper service must be installed as the signed-in user")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return ServiceConfig{}, err
		}
		exePath, err = installDesktopHelper(exePath, home)
		if err != nil {
			return ServiceConfig{}, fmt.Errorf("failed to install desktop helper: %w", err)
		}
		args = []string{"--no-auto-update", "work"}
	}

	cfg := ServiceConfig{
		ExecPath:    exePath,
		Args:        args,
		Description: DefaultDescription,
		UserMode:    runtime.GOOS != "windows", // default to user mode on Unix
	}
	return cfg, nil
}
