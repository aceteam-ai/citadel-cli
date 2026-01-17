// internal/update/state_test.go
package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultState(t *testing.T) {
	state := defaultState()

	if !state.AutoUpdate {
		t.Error("AutoUpdate should be true by default")
	}

	if state.Channel != "stable" {
		t.Errorf("Channel should be 'stable', got '%s'", state.Channel)
	}

	if !state.LastCheck.IsZero() {
		t.Error("LastCheck should be zero value")
	}
}

func TestShouldCheck(t *testing.T) {
	tests := []struct {
		name       string
		autoUpdate bool
		lastCheck  time.Time
		expected   bool
	}{
		{
			name:       "auto update disabled",
			autoUpdate: false,
			lastCheck:  time.Now().Add(-48 * time.Hour),
			expected:   false,
		},
		{
			name:       "never checked",
			autoUpdate: true,
			lastCheck:  time.Time{},
			expected:   true,
		},
		{
			name:       "checked recently",
			autoUpdate: true,
			lastCheck:  time.Now().Add(-1 * time.Hour),
			expected:   false,
		},
		{
			name:       "checked over 24h ago",
			autoUpdate: true,
			lastCheck:  time.Now().Add(-25 * time.Hour),
			expected:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &State{
				AutoUpdate: tt.autoUpdate,
				LastCheck:  tt.lastCheck,
			}

			got := ShouldCheck(state)
			if got != tt.expected {
				t.Errorf("ShouldCheck() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestUpdateLastCheck(t *testing.T) {
	state := &State{}
	before := time.Now()

	UpdateLastCheck(state)

	after := time.Now()

	if state.LastCheck.Before(before) || state.LastCheck.After(after) {
		t.Errorf("LastCheck should be between %v and %v, got %v", before, after, state.LastCheck)
	}
}

func TestRecordUpdate(t *testing.T) {
	state := &State{
		CurrentVersion: "v1.0.0",
	}

	before := time.Now()
	RecordUpdate(state, "v1.0.0", "v1.1.0")
	after := time.Now()

	if state.CurrentVersion != "v1.1.0" {
		t.Errorf("CurrentVersion should be 'v1.1.0', got '%s'", state.CurrentVersion)
	}

	if state.PreviousVersion != "v1.0.0" {
		t.Errorf("PreviousVersion should be 'v1.0.0', got '%s'", state.PreviousVersion)
	}

	if state.LastUpdate.Before(before) || state.LastUpdate.After(after) {
		t.Errorf("LastUpdate should be between %v and %v, got %v", before, after, state.LastUpdate)
	}
}

func TestSaveAndLoadState(t *testing.T) {
	// Create a temp directory for testing
	tempDir, err := os.MkdirTemp("", "citadel-update-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// Override the home directory for testing
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tempDir)
	defer os.Setenv("HOME", origHome)

	// Create the expected directory structure
	updateDir := filepath.Join(tempDir, "citadel-node", "update")
	if err := os.MkdirAll(updateDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a state to save
	originalState := &State{
		CurrentVersion:  "v1.2.3",
		PreviousVersion: "v1.2.2",
		LastCheck:       time.Now().Truncate(time.Second),
		AutoUpdate:      true,
		Channel:         "stable",
	}

	// Save the state
	if err := SaveState(originalState); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	// Load the state
	loadedState, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}

	// Compare
	if loadedState.CurrentVersion != originalState.CurrentVersion {
		t.Errorf("CurrentVersion mismatch: got %s, want %s", loadedState.CurrentVersion, originalState.CurrentVersion)
	}

	if loadedState.PreviousVersion != originalState.PreviousVersion {
		t.Errorf("PreviousVersion mismatch: got %s, want %s", loadedState.PreviousVersion, originalState.PreviousVersion)
	}

	if loadedState.AutoUpdate != originalState.AutoUpdate {
		t.Errorf("AutoUpdate mismatch: got %v, want %v", loadedState.AutoUpdate, originalState.AutoUpdate)
	}

	if loadedState.Channel != originalState.Channel {
		t.Errorf("Channel mismatch: got %s, want %s", loadedState.Channel, originalState.Channel)
	}
}

func TestLoadStateNotFound(t *testing.T) {
	// Create a temp directory with no state file
	tempDir, err := os.MkdirTemp("", "citadel-update-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// Override the home directory
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tempDir)
	defer os.Setenv("HOME", origHome)

	// Load state should return default state when file doesn't exist
	state, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState should not fail for missing file: %v", err)
	}

	if !state.AutoUpdate {
		t.Error("Default state should have AutoUpdate enabled")
	}

	if state.Channel != "stable" {
		t.Errorf("Default state should have channel 'stable', got '%s'", state.Channel)
	}
}
