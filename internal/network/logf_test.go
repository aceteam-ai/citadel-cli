// internal/network/logf_test.go
package network

import (
	"fmt"
	"testing"
)

// TestSetLogf verifies the settable logger can be wired up and receives messages.
func TestSetLogf(t *testing.T) {
	var captured []string
	SetLogf(func(format string, args ...any) {
		captured = append(captured, fmt.Sprintf(format, args...))
	})

	// Simulate a log call
	logf("vpn: state %s -> %s", "NoState", "NeedsLogin")
	logf("vpn: timed out waiting for connection (last state: %s)", "NeedsLogin")

	if len(captured) != 2 {
		t.Fatalf("expected 2 log messages, got %d", len(captured))
	}
	if captured[0] != "vpn: state NoState -> NeedsLogin" {
		t.Errorf("unexpected message [0]: %q", captured[0])
	}
	if captured[1] != "vpn: timed out waiting for connection (last state: NeedsLogin)" {
		t.Errorf("unexpected message [1]: %q", captured[1])
	}

	// Restore default no-op logger
	SetLogf(func(string, ...any) {})
}

// TestSetLogfNilSafe verifies that passing nil to SetLogf is safe (keeps existing logger).
func TestSetLogfNilSafe(t *testing.T) {
	// Should not panic
	SetLogf(nil)

	// logf should still be callable (no-op or previous value)
	logf("this should not panic")
}

// TestLogfDefaultIsNoop verifies the default logf does not panic.
func TestLogfDefaultIsNoop(t *testing.T) {
	// Reset to a known no-op
	SetLogf(func(string, ...any) {})

	// Should not panic
	logf("test message: %s %d", "hello", 42)
}
