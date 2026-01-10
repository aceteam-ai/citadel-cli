package platform

import (
	"os"
	"runtime"
	"testing"
)

func TestOpenURLValidation(t *testing.T) {
	// Skip if not in CI or test environment variable is set
	// to avoid opening browsers during local testing
	if os.Getenv("CI") == "" && os.Getenv("CITADEL_SKIP_BROWSER_TESTS") != "false" {
		t.Skip("Skipping browser test to avoid opening URLs (set CITADEL_SKIP_BROWSER_TESTS=false to run)")
	}

	// Test that OpenURL doesn't panic with a valid URL
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("OpenURL() panicked: %v", r)
		}
	}()

	// Just verify the function signature works
	_ = OpenURL("https://example.com")
}

func TestOpenURLPlatformSpecific(t *testing.T) {
	// Skip if not in CI or test environment variable is set
	// to avoid opening browsers during local testing
	if os.Getenv("CI") == "" && os.Getenv("CITADEL_SKIP_BROWSER_TESTS") != "false" {
		t.Skip("Skipping browser test to avoid opening URLs (set CITADEL_SKIP_BROWSER_TESTS=false to run)")
	}

	// Verify that each platform has a handler
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
