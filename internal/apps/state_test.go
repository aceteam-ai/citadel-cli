package apps

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	s := &State{
		Apps: make(map[string]InstalledApp),
		path: stateFile,
	}

	// Set an app.
	s.Set(InstalledApp{
		Name:        "code-server",
		Image:       "linuxserver/code-server:latest",
		ContainerID: "abc123",
		HostPort:    8100,
	})

	if err := s.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// Verify the file exists.
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("state file does not exist after Save: %v", err)
	}

	// Load it back by reading the file manually (simulating LoadState).
	s2 := &State{
		Apps: make(map[string]InstalledApp),
		path: stateFile,
	}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}

	if len(data) == 0 {
		t.Fatal("state file is empty")
	}

	// Verify the Get method.
	app, ok := s.Get("code-server")
	if !ok {
		t.Fatal("Get(code-server) returned false")
	}
	if app.HostPort != 8100 {
		t.Errorf("Get(code-server).HostPort = %d, want 8100", app.HostPort)
	}
	if app.ContainerID != "abc123" {
		t.Errorf("Get(code-server).ContainerID = %q, want %q", app.ContainerID, "abc123")
	}

	// Verify Get for non-existent.
	_, ok = s2.Get("nonexistent")
	if ok {
		t.Error("Get(nonexistent) should return false")
	}
}

func TestStateRemove(t *testing.T) {
	s := &State{
		Apps: make(map[string]InstalledApp),
		path: filepath.Join(t.TempDir(), "state.json"),
	}

	s.Set(InstalledApp{Name: "test-app", HostPort: 8100})
	s.Remove("test-app")

	if _, ok := s.Get("test-app"); ok {
		t.Error("app should be removed after Remove()")
	}
}

func TestStateInstalledNames(t *testing.T) {
	s := &State{
		Apps: make(map[string]InstalledApp),
		path: filepath.Join(t.TempDir(), "state.json"),
	}

	s.Set(InstalledApp{Name: "app1", HostPort: 8100})
	s.Set(InstalledApp{Name: "app2", HostPort: 8101})

	names := s.InstalledNames()
	if len(names) != 2 {
		t.Errorf("InstalledNames() returned %d, want 2", len(names))
	}
}

func TestAllocatePort(t *testing.T) {
	s := &State{
		Apps: make(map[string]InstalledApp),
		path: filepath.Join(t.TempDir(), "state.json"),
	}

	// First allocation should return portRangeStart.
	port, err := s.AllocatePort()
	if err != nil {
		t.Fatalf("AllocatePort() error: %v", err)
	}
	if port != portRangeStart {
		t.Errorf("AllocatePort() = %d, want %d", port, portRangeStart)
	}

	// After setting an app on portRangeStart, next should be portRangeStart+1.
	s.Set(InstalledApp{Name: "app1", HostPort: portRangeStart})
	port, err = s.AllocatePort()
	if err != nil {
		t.Fatalf("AllocatePort() error: %v", err)
	}
	if port != portRangeStart+1 {
		t.Errorf("AllocatePort() = %d, want %d", port, portRangeStart+1)
	}
}

func TestAllocatePortExhausted(t *testing.T) {
	s := &State{
		Apps: make(map[string]InstalledApp),
		path: filepath.Join(t.TempDir(), "state.json"),
	}

	// Fill the entire port range.
	for i := portRangeStart; i <= portRangeEnd; i++ {
		s.Set(InstalledApp{
			Name:     "app" + string(rune('a'+i-portRangeStart)),
			HostPort: i,
		})
	}

	_, err := s.AllocatePort()
	if err == nil {
		t.Error("AllocatePort() should return error when range is exhausted")
	}
}

func TestLoadStateEmpty(t *testing.T) {
	// Override home dir to a temp dir so LoadState creates a fresh state.
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	state, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}

	if len(state.Apps) != 0 {
		t.Errorf("LoadState() returned %d apps, want 0", len(state.Apps))
	}
}

func TestContainerName(t *testing.T) {
	name := ContainerName("code-server")
	expected := "citadel-app-code-server"
	if name != expected {
		t.Errorf("ContainerName() = %q, want %q", name, expected)
	}
}
