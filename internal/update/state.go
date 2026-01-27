// internal/update/state.go
// State management for auto-update feature
package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// State represents the persistent update state stored in state.json
type State struct {
	CurrentVersion  string    `json:"current_version"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	AvailableUpdate string    `json:"available_update,omitempty"` // Cached latest version for instant notification
	LastCheck       time.Time `json:"last_check"`
	LastUpdate      time.Time `json:"last_update,omitzero"`
	AutoUpdate      bool      `json:"auto_update"`
	Channel         string    `json:"channel"` // "stable" or "rc"
}

// DefaultCheckInterval is the minimum time between update checks
const DefaultCheckInterval = 24 * time.Hour

// GetUpdateDir returns the directory for update-related files
// ~/.citadel-node/update/
func GetUpdateDir() string {
	return filepath.Join(getUserHomeDir(), "citadel-node", "update")
}

// GetStateFilePath returns the path to state.json
func GetStateFilePath() string {
	return filepath.Join(GetUpdateDir(), "state.json")
}

// GetPreviousBinaryPath returns the path to the backup binary
func GetPreviousBinaryPath() string {
	binaryName := "citadel.previous"
	if runtime.GOOS == "windows" {
		binaryName = "citadel.previous.exe"
	}
	return filepath.Join(GetUpdateDir(), binaryName)
}

// GetPendingBinaryPath returns the path to the downloaded pending update
func GetPendingBinaryPath() string {
	binaryName := "citadel.pending"
	if runtime.GOOS == "windows" {
		binaryName = "citadel.pending.exe"
	}
	return filepath.Join(GetUpdateDir(), binaryName)
}

// EnsureUpdateDir creates the update directory if it doesn't exist
func EnsureUpdateDir() error {
	return os.MkdirAll(GetUpdateDir(), 0755)
}

// LoadState reads the update state from disk
// Returns default state if file doesn't exist
func LoadState() (*State, error) {
	statePath := GetStateFilePath()
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultState(), nil
		}
		return nil, err
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return defaultState(), nil
	}

	return &state, nil
}

// SaveState writes the update state to disk
func SaveState(state *State) error {
	if err := EnsureUpdateDir(); err != nil {
		return err
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(GetStateFilePath(), data, 0644)
}

// ShouldCheck returns true if enough time has passed since the last check
func ShouldCheck(state *State) bool {
	if !state.AutoUpdate {
		return false
	}
	return time.Since(state.LastCheck) >= DefaultCheckInterval
}

// UpdateLastCheck updates the last check timestamp
func UpdateLastCheck(state *State) {
	state.LastCheck = time.Now()
}

// RecordUpdate records a successful update
func RecordUpdate(state *State, oldVersion, newVersion string) {
	state.PreviousVersion = oldVersion
	state.CurrentVersion = newVersion
	state.LastUpdate = time.Now()
}

// defaultState returns a new state with sensible defaults
func defaultState() *State {
	return &State{
		AutoUpdate: true,
		Channel:    "stable",
	}
}

// getUserHomeDir returns the user's home directory
func getUserHomeDir() string {
	if runtime.GOOS == "windows" {
		baseDir := os.Getenv("USERPROFILE")
		if baseDir == "" {
			baseDir = os.Getenv("HOME")
		}
		return baseDir
	}

	baseDir, err := os.UserHomeDir()
	if err != nil {
		baseDir = os.Getenv("HOME")
	}
	return baseDir
}
