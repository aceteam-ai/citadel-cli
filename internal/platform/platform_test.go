package platform

import (
	"os"
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
