package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOS(t *testing.T) {
	os := OS()
	if os == "" {
		t.Error("OS() returned empty string")
	}

	// Should match runtime.GOOS
	if os != runtime.GOOS {
		t.Errorf("OS() = %s, want %s", os, runtime.GOOS)
	}
}

func TestIsLinux(t *testing.T) {
	result := IsLinux()
	expected := runtime.GOOS == "linux"
	if result != expected {
		t.Errorf("IsLinux() = %v, want %v", result, expected)
	}
}

func TestIsDarwin(t *testing.T) {
	result := IsDarwin()
	expected := runtime.GOOS == "darwin"
	if result != expected {
		t.Errorf("IsDarwin() = %v, want %v", result, expected)
	}
}

func TestIsWindows(t *testing.T) {
	result := IsWindows()
	expected := runtime.GOOS == "windows"
	if result != expected {
		t.Errorf("IsWindows() = %v, want %v", result, expected)
	}
}

func TestConfigDir(t *testing.T) {
	dir := ConfigDir()

	if dir == "" {
		t.Error("ConfigDir() returned empty string")
	}

	// When not running as root, ConfigDir returns ~/.citadel-cli
	// When running as root, it returns system paths
	if !IsRoot() {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("Failed to get home dir: %v", err)
		}
		expected := home + "/.citadel-cli"
		if dir != expected {
			t.Errorf("ConfigDir() as non-root = %s, want %s", dir, expected)
		}
		return
	}

	// Verify platform-specific system paths (only when running as root)
	switch runtime.GOOS {
	case "linux":
		if dir != "/etc/citadel" {
			t.Errorf("ConfigDir() on Linux as root = %s, want /etc/citadel", dir)
		}
	case "darwin":
		if dir != "/usr/local/etc/citadel" {
			t.Errorf("ConfigDir() on macOS as root = %s, want /usr/local/etc/citadel", dir)
		}
	case "windows":
		if dir != `C:\ProgramData\Citadel` {
			t.Errorf("ConfigDir() on Windows as admin = %s, want C:\\ProgramData\\Citadel", dir)
		}
	}
}

func TestResolveConfigDir_RootWithHOME(t *testing.T) {
	if IsWindows() {
		t.Skip("sudo paths not applicable on Windows")
	}

	// Create a temp dir simulating a user's home with a config file
	tmpHome := t.TempDir()
	configDir := filepath.Join(tmpHome, ".citadel-cli")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("node_config_dir: /tmp\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Simulate root process with HOME pointing at the user's home
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// isRoot=true: should find config via HOME
	result := resolveConfigDir(true, false, true, false)
	expected := filepath.Join(tmpHome, ".citadel-cli")
	if result != expected {
		t.Errorf("resolveConfigDir(root, HOME=%s) = %s, want %s", tmpHome, result, expected)
	}
}

func TestResolveConfigDir_RootWithSUDO_USER(t *testing.T) {
	if IsWindows() {
		t.Skip("sudo paths not applicable on Windows")
	}

	// When HOME doesn't have a config but SUDO_USER's home does,
	// fall back to SUDO_USER's home. We can only test this when running
	// as the current user (SUDO_USER lookup must resolve).
	// For CI safety, just verify that without either signal, system paths win.

	origHome := os.Getenv("HOME")
	t.Setenv("HOME", "/nonexistent-home-for-test")
	defer os.Setenv("HOME", origHome)

	t.Setenv("SUDO_USER", "")

	result := resolveConfigDir(true, false, true, false)
	if result != "/etc/citadel" {
		t.Errorf("resolveConfigDir(root, no HOME config, no SUDO_USER) = %s, want /etc/citadel", result)
	}
}

func TestResolveConfigDir_RootFallbackToSystemPath(t *testing.T) {
	if IsWindows() {
		t.Skip("sudo paths not applicable on Windows")
	}

	// No config in HOME, no SUDO_USER — should fall back to system paths
	t.Setenv("HOME", "/nonexistent-for-test")
	t.Setenv("SUDO_USER", "")

	if IsLinux() {
		result := resolveConfigDir(true, false, true, false)
		if result != "/etc/citadel" {
			t.Errorf("resolveConfigDir(root, Linux) = %s, want /etc/citadel", result)
		}
	}
	if IsDarwin() {
		result := resolveConfigDir(true, false, false, true)
		if result != "/usr/local/etc/citadel" {
			t.Errorf("resolveConfigDir(root, macOS) = %s, want /usr/local/etc/citadel", result)
		}
	}
}

func TestResolveConfigDir_NotRoot(t *testing.T) {
	if IsWindows() {
		t.Skip("Different paths on Windows")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("Cannot determine home dir")
	}

	result := resolveConfigDir(false, false, true, false)
	expected := filepath.Join(home, ".citadel-cli")
	if result != expected {
		t.Errorf("resolveConfigDir(non-root) = %s, want %s", result, expected)
	}
}

func TestConfigDirCandidates_NotRoot(t *testing.T) {
	if IsWindows() {
		t.Skip("sudo/system paths not applicable on Windows")
	}
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("SUDO_USER", "")

	got := configDirCandidates(false, false, true, false)
	want := []string{filepath.Join(tmpHome, ".citadel-cli"), "/etc/citadel"}
	assertStringSlicesEqual(t, got, want)
}

func TestConfigDirCandidates_RootHOMEForwarded(t *testing.T) {
	if IsWindows() {
		t.Skip("sudo/system paths not applicable on Windows")
	}
	tmpHome := t.TempDir()
	configDir := filepath.Join(tmpHome, ".citadel-cli")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("node_config_dir: /tmp\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpHome)
	t.Setenv("SUDO_USER", "")

	got := configDirCandidates(true, false, true, false)
	want := []string{filepath.Join(tmpHome, ".citadel-cli"), "/etc/citadel"}
	assertStringSlicesEqual(t, got, want)
}

func TestConfigDirCandidates_RootNoContextIncludesSystemPathAndHOME(t *testing.T) {
	if IsWindows() {
		t.Skip("sudo/system paths not applicable on Windows")
	}
	// Simulates the shipped systemd unit (HOME set, but with no config.yaml
	// underneath it, and no SUDO_USER): resolveConfigDir falls through to
	// /etc/citadel, but the candidate list must ALSO keep the HOME-derived
	// dir in case a legacy hash was written there directly (e.g. by an
	// earlier binary/config layout). Uses a synthetic nonexistent path (like
	// TestResolveConfigDir_RootFallbackToSystemPath) so the test result does
	// not depend on what happens to exist on the machine running it.
	t.Setenv("HOME", "/nonexistent-home-for-candidates-test")
	t.Setenv("SUDO_USER", "")

	got := configDirCandidates(true, false, true, false)
	want := []string{"/etc/citadel", "/nonexistent-home-for-candidates-test/.citadel-cli"}
	assertStringSlicesEqual(t, got, want)
}

func TestConfigDirCandidates_RootDarwinSystemPath(t *testing.T) {
	t.Setenv("HOME", "/nonexistent-home-for-candidates-test")
	t.Setenv("SUDO_USER", "")

	got := configDirCandidates(true, false, false, true)
	want := []string{"/usr/local/etc/citadel", "/nonexistent-home-for-candidates-test/.citadel-cli"}
	assertStringSlicesEqual(t, got, want)
}

func TestConfigDirCandidates_Windows(t *testing.T) {
	// Windows ConfigDir() is invoker-invariant, so there is exactly one
	// candidate: whatever resolveConfigDir itself returns.
	got := configDirCandidates(true, true, false, false)
	if len(got) != 1 {
		t.Fatalf("Windows candidates = %v, want exactly 1 entry", got)
	}
}

func TestConfigDirCandidates_NoDuplicates(t *testing.T) {
	if IsWindows() {
		t.Skip("sudo/system paths not applicable on Windows")
	}
	// HOME equal to the process's real home is the common case: every source
	// (resolveConfigDir, os.UserHomeDir, explicit $HOME) agrees, so the list
	// must collapse to the system path plus exactly one home-derived entry.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")

	got := configDirCandidates(false, false, true, false)
	seen := map[string]bool{}
	for _, d := range got {
		if seen[d] {
			t.Fatalf("duplicate candidate %q in %v", d, got)
		}
		seen[d] = true
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %v, want exactly 2 deduplicated entries", got)
	}
}

func assertStringSlicesEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestDefaultNodeDir(t *testing.T) {
	// Test with current user
	dir, err := DefaultNodeDir("")
	if err != nil {
		t.Fatalf("DefaultNodeDir() error = %v", err)
	}

	if dir == "" {
		t.Error("DefaultNodeDir() returned empty string")
	}

	// Should contain "citadel-node"
	if !contains(dir, "citadel-node") {
		t.Errorf("DefaultNodeDir() = %s, should contain 'citadel-node'", dir)
	}
}

func TestGetUIDWindows(t *testing.T) {
	if !IsWindows() {
		t.Skip("Skipping Windows-specific test")
	}

	// On Windows, GetUID should return empty string
	uid, err := GetUID("testuser")
	if err != nil {
		t.Errorf("GetUID() on Windows should not error, got: %v", err)
	}
	if uid != "" {
		t.Errorf("GetUID() on Windows = %s, want empty string", uid)
	}
}

func TestGetGIDWindows(t *testing.T) {
	if !IsWindows() {
		t.Skip("Skipping Windows-specific test")
	}

	// On Windows, GetGID should return empty string
	gid, err := GetGID("testuser")
	if err != nil {
		t.Errorf("GetGID() on Windows should not error, got: %v", err)
	}
	if gid != "" {
		t.Errorf("GetGID() on Windows = %s, want empty string", gid)
	}
}

func TestChownRWindows(t *testing.T) {
	if !IsWindows() {
		t.Skip("Skipping Windows-specific test")
	}

	// On Windows, ChownR should be a no-op
	err := ChownR("/some/path", "testuser")
	if err != nil {
		t.Errorf("ChownR() on Windows should not error, got: %v", err)
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || containsMiddle(s, substr)))
}

func containsMiddle(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
