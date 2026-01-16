// internal/network/state.go
// State directory management for tsnet network connections
package network

import (
	"os"
	"path/filepath"
	"runtime"
)

// GetStateDir returns the state directory path for network state.
// This is where tsnet stores WireGuard keys and connection state.
//
// Locations:
//   - Linux/macOS: ~/.citadel-node/network/
//   - Windows: %USERPROFILE%\citadel-node\network\
func GetStateDir() string {
	var baseDir string

	if runtime.GOOS == "windows" {
		baseDir = os.Getenv("USERPROFILE")
		if baseDir == "" {
			baseDir = os.Getenv("HOME")
		}
	} else {
		var err error
		baseDir, err = os.UserHomeDir()
		if err != nil {
			baseDir = os.Getenv("HOME")
		}
	}

	return filepath.Join(baseDir, "citadel-node", "network")
}

// EnsureStateDir creates the state directory if it doesn't exist.
// Returns the state directory path.
func EnsureStateDir() (string, error) {
	stateDir := GetStateDir()
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return "", err
	}
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
