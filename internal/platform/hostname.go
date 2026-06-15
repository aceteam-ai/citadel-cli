package platform

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// GenerateCitadelHostname generates a hostname like "citadel-<short_id>" for use
// during node initialization. The short_id is derived from /etc/machine-id (first
// 8 characters) when available, or a random 8-character hex string as fallback.
//
// This ensures nodes get unique, recognizable names instead of inheriting generic
// OS hostnames like "debian" on live ISOs.
func GenerateCitadelHostname() (string, error) {
	shortID, err := getShortMachineID()
	if err != nil {
		shortID, err = generateRandomHexID(8)
		if err != nil {
			return "", fmt.Errorf("failed to generate hostname: %w", err)
		}
	}
	return "citadel-" + shortID, nil
}

// getShortMachineID reads /etc/machine-id and returns its first 8 characters.
// Returns an error if the file is missing, empty, or shorter than 8 characters.
func getShortMachineID() (string, error) {
	data, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return "", fmt.Errorf("could not read /etc/machine-id: %w", err)
	}

	id := strings.TrimSpace(string(data))
	if len(id) < 8 {
		return "", fmt.Errorf("/etc/machine-id too short: %q", id)
	}

	return id[:8], nil
}

// generateRandomHexID generates a random hex string of the specified length.
func generateRandomHexID(length int) (string, error) {
	// We need length/2 bytes (rounded up) to produce `length` hex chars
	numBytes := (length + 1) / 2
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return hex.EncodeToString(b)[:length], nil
}

// SetHostname attempts to set the system hostname. This is best-effort and will
// return an error if it fails (e.g., when not running as root). Callers should
// treat failure as non-fatal — the generated hostname is still used for Headscale
// registration regardless of whether the OS hostname is updated.
//
// On Linux: tries hostnamectl, falls back to writing /etc/hostname + syscall.
// On other platforms: no-op (returns nil).
func SetHostname(name string) error {
	if !IsLinux() {
		return nil
	}

	// Try hostnamectl first (sets both transient and static hostname)
	if err := exec.Command("hostnamectl", "set-hostname", name).Run(); err == nil {
		return nil
	}

	// Fallback: write /etc/hostname for persistence across reboots
	if err := os.WriteFile("/etc/hostname", []byte(name+"\n"), 0644); err != nil {
		return fmt.Errorf("failed to write /etc/hostname: %w", err)
	}

	// Also set the transient hostname for the current session
	if err := exec.Command("hostname", name).Run(); err != nil {
		return fmt.Errorf("failed to set transient hostname: %w", err)
	}

	return nil
}
