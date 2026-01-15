package platform

import (
	"fmt"
	"os"
	"os/exec"
)

// GetTailscaleCLI returns the path to the tailscale CLI executable.
// It checks PATH first, then platform-specific locations:
// - Windows: C:\Program Files\Tailscale\tailscale.exe
// - macOS: Homebrew paths (Apple Silicon and Intel) and App Store location
// - Linux: Usually in PATH, no special handling needed
func GetTailscaleCLI() string {
	// Check PATH first
	if path, err := exec.LookPath("tailscale"); err == nil {
		return path
	}

	// Windows: Check standard installation path
	if IsWindows() {
		paths := []string{
			`C:\Program Files\Tailscale\tailscale.exe`,
		}
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}

	// macOS: Check Homebrew and App Store locations
	if IsDarwin() {
		paths := []string{
			"/opt/homebrew/bin/tailscale",                          // Homebrew (Apple Silicon)
			"/usr/local/bin/tailscale",                             // Homebrew (Intel)
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale", // App Store
		}
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}

	return "tailscale" // Fallback (Linux usually has it in PATH)
}

// IsTailscaleInstalled checks if Tailscale is installed on the system.
// Returns true if the tailscale binary is found in PATH or at known platform-specific locations.
func IsTailscaleInstalled() bool {
	// Check PATH first
	if _, err := exec.LookPath("tailscale"); err == nil {
		return true
	}

	// Windows: Check standard installation path
	if IsWindows() {
		if _, err := os.Stat(`C:\Program Files\Tailscale\tailscale.exe`); err == nil {
			return true
		}
	}

	// macOS: Check Homebrew and App Store locations
	if IsDarwin() {
		paths := []string{
			"/opt/homebrew/bin/tailscale",
			"/usr/local/bin/tailscale",
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
		}
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				return true
			}
		}
	}

	return false
}

// CheckTailscaleCLIEnabled verifies the Tailscale CLI is functional.
// On macOS with App Store installation, CLI must be manually enabled in
// Tailscale > Settings > CLI.
func CheckTailscaleCLIEnabled() error {
	cli := GetTailscaleCLI()
	cmd := exec.Command(cli, "version")
	if err := cmd.Run(); err != nil {
		if IsDarwin() {
			return fmt.Errorf("Tailscale CLI not available. If you installed Tailscale from the App Store, " +
				"please open Tailscale > Settings and enable 'Use Tailscale CLI'")
		}
		return fmt.Errorf("Tailscale CLI not functional: %w", err)
	}
	return nil
}
