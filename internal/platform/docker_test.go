package platform

import (
	"runtime"
	"testing"
)

func TestGetDockerManager(t *testing.T) {
	dm, err := GetDockerManager()
	if err != nil {
		t.Fatalf("GetDockerManager() error = %v", err)
	}

	if dm == nil {
		t.Fatal("GetDockerManager() returned nil")
	}

	// Just verify we can get a manager for each platform
	switch runtime.GOOS {
	case "linux":
		_, ok := dm.(*LinuxDockerManager)
		if !ok {
			t.Errorf("GetDockerManager() on Linux did not return LinuxDockerManager")
		}
	case "darwin":
		_, ok := dm.(*DarwinDockerManager)
		if !ok {
			t.Errorf("GetDockerManager() on macOS did not return DarwinDockerManager")
		}
	case "windows":
		_, ok := dm.(*WindowsDockerManager)
		if !ok {
			t.Errorf("GetDockerManager() on Windows did not return WindowsDockerManager")
		}
	}
}

func TestWindowsDockerManagerNoOps(t *testing.T) {
	if !IsWindows() {
		t.Skip("Skipping Windows-specific test")
	}

	dm := &WindowsDockerManager{}

	// EnsureUserInDockerGroup should be a no-op
	err := dm.EnsureUserInDockerGroup("testuser")
	if err != nil {
		t.Errorf("WindowsDockerManager.EnsureUserInDockerGroup() error = %v, want nil", err)
	}

	// ConfigureRuntime should be a no-op
	err = dm.ConfigureRuntime()
	if err != nil {
		t.Errorf("WindowsDockerManager.ConfigureRuntime() error = %v, want nil", err)
	}
}

func TestWindowsDockerManagerWSLDetection(t *testing.T) {
	if !IsWindows() {
		t.Skip("Skipping Windows-specific test")
	}

	dm := &WindowsDockerManager{}

	// Test hasWSL2 - should not panic even if WSL not installed
	hasWSL2 := dm.hasWSL2()
	t.Logf("hasWSL2() = %v", hasWSL2)

	// Test getWSLStatus - should return one of the valid status strings
	status := dm.getWSLStatus()
	t.Logf("getWSLStatus() = %s", status)

	validStatuses := map[string]bool{
		"wsl_not_installed": true,
		"wsl_command_failed": true,
		"wsl1_only":         true,
		"wsl2_installed":    true,
		"no_distributions":  true,
	}

	if !validStatuses[status] {
		t.Errorf("getWSLStatus() returned unexpected status: %s", status)
	}

	// If hasWSL2 is true, status should be wsl2_installed
	if hasWSL2 && status != "wsl2_installed" {
		t.Errorf("hasWSL2() = true but getWSLStatus() = %s, want wsl2_installed", status)
	}
}
