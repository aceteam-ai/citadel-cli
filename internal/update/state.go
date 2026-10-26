// internal/update/state.go
// State management for auto-update feature
package update

import (
	"encoding/json"
	"errors"
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

	// Telemetry fields for the heartbeat auto_update report (citadel-cli#1134).
	// Written on every attempted check and terminal result through the atomic
	// mutateState path (report.go), which only ever touches these fields so a
	// concurrent preference write (AutoUpdate, via `citadel update
	// enable/disable`) is never clobbered. Additive and omitzero/omitempty:
	// older state.json files lack them and read as zero (degraded to
	// "unknown"/"never_checked" by the report builder), keeping this struct
	// JSON-compatible with every pre-#1134 state file.
	LastCheckAt   time.Time `json:"last_check_at,omitzero"`   // last release check actually attempted
	LatestVersion string    `json:"latest_version,omitempty"` // last release observed by this node
	LastResultAt  time.Time `json:"last_result_at,omitzero"`  // when LastResult was recorded
	LastResult    string    `json:"last_result,omitempty"`    // bounded result code (see report.go)
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
// Returns default state only if the file does not exist. Malformed or
// unreadable state returns an error and is never implicitly repaired.
func LoadState() (*State, error) {
	return loadStateFile(GetStateFilePath())
}

// ErrStateCorrupt requires an explicit preference repair, never a telemetry write.
var ErrStateCorrupt = errors.New("update state is corrupt")

func loadStateFile(statePath string) (*State, error) {
	info, err := os.Lstat(statePath)
	if os.IsNotExist(err) {
		return defaultState(), nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("update state is not a regular file")
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultState(), nil
		}
		return nil, err
	}

	var state *State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, ErrStateCorrupt
	}
	if state == nil {
		return nil, ErrStateCorrupt
	}
	return state, nil
}

// SaveState atomically replaces a full snapshot under the cross-process lock.
// Load/modify/save callers must use MutateState to avoid saving stale fields.
func SaveState(state *State) error {
	path := GetStateFilePath()
	return withStateLock(path, func() error { return writeStateFile(path, state, defaultStateFileOps()) })
}

// MutateState serializes the complete transaction across processes. The callback
// must be short and must not call another state writer. Unreadable or corrupt
// state is preserved rather than silently resetting preferences or results.
func MutateState(mutate func(*State)) error {
	return changeState(false, mutate)
}

// SetAutoUpdate is the explicit preference operation and the only automatic-
// updater API that repairs malformed JSON. No prior fields are guessed or
// salvaged; unreadable files still return an error.
func SetAutoUpdate(enabled bool) error {
	return changeState(true, func(state *State) { state.AutoUpdate = enabled })
}

func changeState(repair bool, mutate func(*State)) error {
	if mutate == nil {
		return errors.New("missing update state mutation")
	}
	path := GetStateFilePath()
	return withStateLock(path, func() error {
		state, err := loadStateFile(path)
		if err != nil {
			if !repair || !errors.Is(err, ErrStateCorrupt) {
				return err
			}
			state = defaultState()
		}
		mutate(state)
		return writeStateFile(path, state, defaultStateFileOps())
	})
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

// defaultState returns a new state with sensible defaults.
// AutoUpdate defaults to false: auto-installing a new binary on a node is
// consequential, so it is opt-in (turned on explicitly via `citadel update
// enable` or the web UI), never on by default.
func defaultState() *State {
	return &State{
		AutoUpdate: false,
		Channel:    "stable",
	}
}

// getUserHomeDir returns the user's home directory.
// On Windows, uses LOCALAPPDATA/APPDATA to avoid resolving to
// C:\Windows\System32\config\systemprofile when running as SYSTEM.
func getUserHomeDir() string {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return v
		}
		if v := os.Getenv("APPDATA"); v != "" {
			return v
		}
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return os.Getenv("USERPROFILE")
	}

	baseDir, err := os.UserHomeDir()
	if err != nil {
		baseDir = os.Getenv("HOME")
	}
	return baseDir
}
