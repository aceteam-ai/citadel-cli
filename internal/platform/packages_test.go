package platform

import (
	"runtime"
	"testing"
)

func TestGetPackageManager(t *testing.T) {
	pm, err := GetPackageManager()
	if err != nil {
		t.Fatalf("GetPackageManager() error = %v", err)
	}

	if pm == nil {
		t.Fatal("GetPackageManager() returned nil")
	}

	// Verify correct manager for platform
	name := pm.Name()
	switch runtime.GOOS {
	case "linux":
		if name != "apt" {
			t.Errorf("GetPackageManager() on Linux returned %s, want apt", name)
		}
	case "darwin":
		if name != "brew" {
			t.Errorf("GetPackageManager() on macOS returned %s, want brew", name)
		}
	case "windows":
		if name != "winget" {
			t.Errorf("GetPackageManager() on Windows returned %s, want winget", name)
		}
	}
}

func TestWingetPackageManagerName(t *testing.T) {
	if !IsWindows() {
		t.Skip("Skipping Windows-specific test")
	}

	pm := &WingetPackageManager{}
	if pm.Name() != "winget" {
		t.Errorf("WingetPackageManager.Name() = %s, want winget", pm.Name())
	}
}

func TestAptPackageManagerName(t *testing.T) {
	if !IsLinux() {
		t.Skip("Skipping Linux-specific test")
	}

	pm := &AptPackageManager{}
	if pm.Name() != "apt" {
		t.Errorf("AptPackageManager.Name() = %s, want apt", pm.Name())
	}
}

func TestBrewPackageManagerName(t *testing.T) {
	if !IsDarwin() {
		t.Skip("Skipping macOS-specific test")
	}

	pm := &BrewPackageManager{}
	if pm.Name() != "brew" {
		t.Errorf("BrewPackageManager.Name() = %s, want brew", pm.Name())
	}
}
