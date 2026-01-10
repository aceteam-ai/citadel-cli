package platform

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	case "windows":
		return &WingetPackageManager{}, nil
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
	// Try up to 3 times with a 3-second delay between attempts
	// This handles cases where apt lock files are held by another process
	maxRetries := 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		cmd := exec.Command("apt-get", "update", "-qq")
		output, err := cmd.CombinedOutput()

		if err == nil {
			return nil
		}

		// Check if it's a lock error (exit status 100)
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 100 {
			if attempt < maxRetries {
				fmt.Fprintf(os.Stderr, "     - apt is locked, waiting 3 seconds (attempt %d/%d)...\n", attempt, maxRetries)
				exec.Command("sleep", "3").Run()
				continue
			}
			return fmt.Errorf("apt-get update failed after %d attempts (lock held): %w\n%s", maxRetries, err, string(output))
		}

		// For other errors, return immediately with the full output
		if len(output) > 0 {
			return fmt.Errorf("apt-get update failed: %w\n%s", err, string(output))
		}
		return err
	}
	return fmt.Errorf("apt-get update failed after %d attempts", maxRetries)
}

func (a *AptPackageManager) Install(packages ...string) error {
	args := append([]string{"install", "-y", "-qq"}, packages...)

	// Try up to 3 times with a 3-second delay between attempts
	maxRetries := 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		cmd := exec.Command("apt-get", args...)
		// Set DEBIAN_FRONTEND=noninteractive to avoid any prompts
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		output, err := cmd.CombinedOutput()

		if err == nil {
			return nil
		}

		// Check if it's a lock error (exit status 100)
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 100 {
			if attempt < maxRetries {
				fmt.Fprintf(os.Stderr, "     - apt is locked, waiting 3 seconds (attempt %d/%d)...\n", attempt, maxRetries)
				exec.Command("sleep", "3").Run()
				continue
			}
			return fmt.Errorf("apt-get install failed after %d attempts (lock held): %w\n%s", maxRetries, err, string(output))
		}

		// For other errors, return immediately with the full output
		if len(output) > 0 {
			return fmt.Errorf("apt-get install failed: %w\n%s", err, string(output))
		}
		return err
	}
	return fmt.Errorf("apt-get install failed after %d attempts", maxRetries)
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

// WingetPackageManager implements PackageManager for Windows Package Manager (winget)
type WingetPackageManager struct{}

func (w *WingetPackageManager) Name() string {
	return "winget"
}

func (w *WingetPackageManager) Update() error {
	// Update winget sources
	cmd := exec.Command("winget", "source", "update")
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Don't fail if update fails - winget can work without explicit updates
	_ = cmd.Run()
	return nil
}

func (w *WingetPackageManager) Install(packages ...string) error {
	for _, pkg := range packages {
		if w.IsInstalled(pkg) {
			continue // Skip already installed packages
		}

		cmd := exec.Command("winget", "install", "--id", pkg, "--silent", "--accept-package-agreements", "--accept-source-agreements")
		output, err := cmd.CombinedOutput()
		if err != nil {
			// Check if error is because package is already installed
			if strings.Contains(string(output), "already installed") {
				continue
			}
			return fmt.Errorf("failed to install %s: %w\n%s", pkg, err, string(output))
		}
	}
	return nil
}

func (w *WingetPackageManager) IsInstalled(pkg string) bool {
	cmd := exec.Command("winget", "list", "--id", pkg, "--exact")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	// Check if the package ID appears in the output (case-insensitive)
	return strings.Contains(strings.ToLower(string(output)), strings.ToLower(pkg))
}

