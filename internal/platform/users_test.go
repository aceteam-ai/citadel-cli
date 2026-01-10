package platform

import (
	"runtime"
	"testing"
)

func TestGetUserManager(t *testing.T) {
	um, err := GetUserManager()
	if err != nil {
		t.Fatalf("GetUserManager() error = %v", err)
	}

	if um == nil {
		t.Fatal("GetUserManager() returned nil")
	}

	// Verify correct manager for platform
	switch runtime.GOOS {
	case "linux":
		_, ok := um.(*LinuxUserManager)
		if !ok {
			t.Errorf("GetUserManager() on Linux did not return LinuxUserManager")
		}
	case "darwin":
		_, ok := um.(*DarwinUserManager)
		if !ok {
			t.Errorf("GetUserManager() on macOS did not return DarwinUserManager")
		}
	case "windows":
		_, ok := um.(*WindowsUserManager)
		if !ok {
			t.Errorf("GetUserManager() on Windows did not return WindowsUserManager")
		}
	}
}

func TestGenerateRandomPassword(t *testing.T) {
	if !IsWindows() {
		t.Skip("Skipping Windows-specific test")
	}

	// Test password generation
	password1, err := generateRandomPassword(16)
	if err != nil {
		t.Fatalf("generateRandomPassword() error = %v", err)
	}

	if len(password1) != 16 {
		t.Errorf("generateRandomPassword(16) returned password of length %d, want 16", len(password1))
	}

	// Generate another password - should be different
	password2, err := generateRandomPassword(16)
	if err != nil {
		t.Fatalf("generateRandomPassword() error = %v", err)
	}

	if password1 == password2 {
		t.Error("generateRandomPassword() generated identical passwords (should be random)")
	}

	// Test different lengths
	password3, err := generateRandomPassword(32)
	if err != nil {
		t.Fatalf("generateRandomPassword(32) error = %v", err)
	}

	if len(password3) != 32 {
		t.Errorf("generateRandomPassword(32) returned password of length %d, want 32", len(password3))
	}
}

func TestWindowsUserManagerUserExists(t *testing.T) {
	if !IsWindows() {
		t.Skip("Skipping Windows-specific test")
	}

	um := &WindowsUserManager{}

	// Test with current user (should exist)
	// We can't test with a specific username as it varies by system
	// Just verify the method doesn't panic
	_ = um.UserExists("Administrator")
}
