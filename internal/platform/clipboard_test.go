package platform

import (
	"runtime"
	"testing"
)

func TestCopyToClipboardValidation(t *testing.T) {
	// Test that CopyToClipboard doesn't panic with valid text
	// We can't actually test clipboard in CI, but we can verify
	// the function exists and handles basic validation

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("CopyToClipboard() panicked: %v", r)
		}
	}()

	// Just verify the function signature works
	_ = CopyToClipboard("test")
}

func TestCopyToClipboardPlatformSpecific(t *testing.T) {
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
