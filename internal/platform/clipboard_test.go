package platform

import (
	"os"
	"runtime"
	"testing"
)

func TestCopyToClipboardValidation(t *testing.T) {
	// Skip if not in CI to avoid modifying clipboard during local testing
	if os.Getenv("CI") == "" && os.Getenv("CITADEL_SKIP_CLIPBOARD_TESTS") != "false" {
		t.Skip("Skipping clipboard test to avoid modifying clipboard (set CITADEL_SKIP_CLIPBOARD_TESTS=false to run)")
	}

	// Test that CopyToClipboard doesn't panic with valid text
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("CopyToClipboard() panicked: %v", r)
		}
	}()

	// Just verify the function signature works
	_ = CopyToClipboard("test")
}

func TestCopyToClipboardPlatformSpecific(t *testing.T) {
	// Skip if not in CI to avoid modifying clipboard during local testing
	if os.Getenv("CI") == "" && os.Getenv("CITADEL_SKIP_CLIPBOARD_TESTS") != "false" {
		t.Skip("Skipping clipboard test to avoid modifying clipboard (set CITADEL_SKIP_CLIPBOARD_TESTS=false to run)")
	}

	// Verify that each platform has a handler
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
		// These platforms are supported
		// The function should at least attempt to copy
		err := CopyToClipboard("test text")
		// We expect an error in test environment (no clipboard)
		// But it should not panic
		_ = err // Error is expected in CI
	default:
		// Unsupported platforms should return an error
		err := CopyToClipboard("test")
		if err == nil {
			t.Error("CopyToClipboard() on unsupported platform should return error")
		}
	}
}
