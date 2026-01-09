package platform

import (
	"fmt"
	"os"
	"os/exec"
)

// PackageManager interface defines operations for system package management
type PackageManager interface {
	Update() error
	Install(packages ...string) error
	IsInstalled(pkg string) bool
	Name() string
}

// GetPackageManager returns the appropriate package manager for the current OS
func GetPackageManager() (PackageManager, error) {
	switch OS() {
	case "linux":
		return &AptPackageManager{}, nil
	case "darwin":
		return &BrewPackageManager{}, nil
	default:
		return nil, fmt.Errorf("unsupported operating system: %s", OS())
	}
}

// AptPackageManager implements PackageManager for Debian/Ubuntu systems
type AptPackageManager struct{}

func (a *AptPackageManager) Name() string {
	return "apt"
}

func (a *AptPackageManager) Update() error {
	cmd := exec.Command("apt-get", "update")
	cmd.Stdout = nil // Silent
	cmd.Stderr = nil
	return cmd.Run()
}

func (a *AptPackageManager) Install(packages ...string) error {
	args := append([]string{"install", "-y"}, packages...)
	cmd := exec.Command("apt-get", args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

func (a *AptPackageManager) IsInstalled(pkg string) bool {
	cmd := exec.Command("dpkg", "-l", pkg)
	return cmd.Run() == nil
}

// BrewPackageManager implements PackageManager for macOS Homebrew
type BrewPackageManager struct{}

func (b *BrewPackageManager) Name() string {
	return "brew"
}

// brewCommand creates a command to run brew, handling the case where we're running as root via sudo.
// Homebrew refuses to run as root, so we need to run as the original user.
func brewCommand(args ...string) *exec.Cmd {
	// If running as root via sudo, run brew as the original user
	if os.Geteuid() == 0 {
		if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
			sudoArgs := append([]string{"-u", sudoUser, "brew"}, args...)
			return exec.Command("sudo", sudoArgs...)
		}
	}
	return exec.Command("brew", args...)
}

func (b *BrewPackageManager) Update() error {
	cmd := brewCommand("update")
	cmd.Stdout = nil // Silent
	cmd.Stderr = nil
	return cmd.Run()
}

func (b *BrewPackageManager) Install(packages ...string) error {
	// brew install will skip already-installed packages automatically
	cmd := brewCommand(append([]string{"install"}, packages...)...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

func (b *BrewPackageManager) IsInstalled(pkg string) bool {
	cmd := brewCommand("list", pkg)
	return cmd.Run() == nil
}

// EnsureHomebrew checks if Homebrew is installed and installs it if not
func EnsureHomebrew() error {
	if !IsDarwin() {
		return fmt.Errorf("Homebrew is only supported on macOS")
	}

	// Check if brew is already installed
	if _, err := exec.LookPath("brew"); err == nil {
		return nil // Already installed
	}

	fmt.Println("Homebrew not found. Installing Homebrew...")
	installScript := `NONINTERACTIVE=1 /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`

	var cmd *exec.Cmd
	// Homebrew installer also refuses to run as root, so run as original user
	if os.Geteuid() == 0 {
		if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
			cmd = exec.Command("sudo", "-u", sudoUser, "bash", "-c", installScript)
		} else {
			return fmt.Errorf("cannot install Homebrew as root without SUDO_USER set")
		}
	} else {
		cmd = exec.Command("bash", "-c", installScript)
	}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to install Homebrew: %w", err)
	}

	return nil
}
