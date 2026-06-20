// Package service provides platform-agnostic managed service installation
// for the Citadel node agent (systemd, launchd, Windows Service).
package service

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
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

	cfg := ServiceConfig{
		ExecPath:    exePath,
		Args:        []string{"work"},
		Description: DefaultDescription,
		UserMode:    runtime.GOOS != "windows", // default to user mode on Unix
	}
	return cfg, nil
}
