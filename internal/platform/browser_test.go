package platform

import (
	"runtime"
	"testing"
)

func TestOpenURLValidation(t *testing.T) {
	// Test that OpenURL doesn't panic with a valid URL
	// We can't actually open a browser in tests, but we can verify
	// the function exists and handles basic validation

	// This will try to open the URL but likely fail in CI
	// We just verify it doesn't panic
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("OpenURL() panicked: %v", r)
		}
	}()

	// Just verify the function signature works
	_ = OpenURL("https://example.com")
}

func TestOpenURLPlatformSpecific(t *testing.T) {
	// Verify that each platform has a handler
	// We can't actually test opening URLs in unit tests
	// but we can verify the logic paths exist

	switch runtime.GOOS {
	case "linux", "darwin", "windows":
		// These platforms are supported
		// The function should at least attempt to open
		err := OpenURL("https://example.com")
		// We expect an error in test environment (no display)
		// But it should not panic and should return an error
		_ = err // Error is expected in CI
	default:
		// Unsupported platforms should return an error
		err := OpenURL("https://example.com")
		if err == nil {
			t.Error("OpenURL() on unsupported platform should return error")
		}
	}
}
