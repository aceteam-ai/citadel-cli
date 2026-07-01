package apps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/aceteam-ai/citadel-cli/services"
)

// InstalledApp records the runtime state of a deployed app.
type InstalledApp struct {
	Name        string `json:"name"`
	Image       string `json:"image"`
	ContainerID string `json:"container_id"`
	HostPort    int    `json:"host_port"`
}

// State persists the set of installed apps and their port assignments.
type State struct {
	Apps map[string]InstalledApp `json:"apps"`

	mu   sync.RWMutex
	path string
}

// appsBaseDir returns ~/.citadel/apps.
func appsBaseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".citadel", "apps"), nil
}

// statePath returns the path to the state file.
func statePath() (string, error) {
	base, err := appsBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "state.json"), nil
}

// AppDir returns the root directory for a specific app (~/.citadel/apps/<name>/).
func AppDir(appName string) (string, error) {
	base, err := appsBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appName), nil
}

// AppDataDir returns the data directory for a specific app.
func AppDataDir(appName string) (string, error) {
	appDir, err := AppDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(appDir, "data"), nil
}

// LoadState reads the state file from disk. If the file does not exist,
// an empty state is returned.
func LoadState() (*State, error) {
	p, err := statePath()
	if err != nil {
		return nil, err
	}

	s := &State{
		Apps: make(map[string]InstalledApp),
		path: p,
	}

	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}

	if err := json.Unmarshal(data, &s.Apps); err != nil {
		return nil, fmt.Errorf("failed to parse state file: %w", err)
	}
	return s, nil
}

// Save writes the state to disk atomically.
func (s *State) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	data, err := json.MarshalIndent(s.Apps, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	// Write to temp file then rename for atomicity.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("failed to finalize state file: %w", err)
	}
	return nil
}

// Get returns the InstalledApp for the given name, or false if not installed.
func (s *State) Get(name string) (InstalledApp, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	app, ok := s.Apps[name]
	return app, ok
}

// Set records an installed app.
func (s *State) Set(app InstalledApp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Apps[app.Name] = app
}

// Remove deletes an app from the state.
func (s *State) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Apps, name)
}

// InstalledNames returns the names of all installed apps in no particular order.
func (s *State) InstalledNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.Apps))
	for k := range s.Apps {
		names = append(names, k)
	}
	return names
}

// portRange defines the auto-allocation range for app host ports.
const (
	portRangeStart = 8100
	portRangeEnd   = 8199
)

// AllocatePort returns the next available port in the 8100-8199 range
// that is not already used by an installed app. It returns an error
// if the range is exhausted. Ports reserved by citadel's own services that
// happen to fall inside this range (e.g. the TEI embedding upstream 8102 and
// the transcribe sidecar 8101) are skipped so a dynamically allocated app can
// never collide with them.
func (s *State) AllocatePort() (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	used := make(map[int]bool, len(s.Apps))
	for _, app := range s.Apps {
		used[app.HostPort] = true
	}
	reserved := services.InRangeReservedHostPorts()

	for port := portRangeStart; port <= portRangeEnd; port++ {
		if !used[port] && !reserved[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available ports in range %d-%d", portRangeStart, portRangeEnd)
}
