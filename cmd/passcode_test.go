// cmd/passcode_test.go
package cmd

import (
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// TestSetNodePasscodePersists confirms 'citadel passcode set' actually
// persists a passcode that VerifyPasscode subsequently accepts (citadel#753).
func TestSetNodePasscodePersists(t *testing.T) {
	dir := t.TempDir()

	perms, err := setNodePasscode(dir, "1234")
	if err != nil {
		t.Fatalf("setNodePasscode: %v", err)
	}
	if !perms.HasPasscode() {
		t.Fatal("expected HasPasscode() true after set")
	}

	// Reload from disk: setNodePasscode must have persisted, not just mutated
	// the in-memory struct.
	reloaded := config.LoadPermissions(dir)
	if !reloaded.HasPasscode() {
		t.Fatal("passcode was not persisted to disk")
	}
	if !reloaded.VerifyPasscode("1234") {
		t.Error("reloaded permissions do not verify the passcode that was set")
	}
	if reloaded.VerifyPasscode("wrong") {
		t.Error("reloaded permissions verified an incorrect passcode")
	}
}

// TestSetNodePasscodeRejectsEmpty confirms an empty/whitespace pin is refused
// (the explicit removal path is 'citadel passcode clear', not an empty set).
func TestSetNodePasscodeRejectsEmpty(t *testing.T) {
	dir := t.TempDir()

	if _, err := setNodePasscode(dir, ""); err == nil {
		t.Error("expected error for empty passcode, got nil")
	}
	if _, err := setNodePasscode(dir, "   "); err == nil {
		t.Error("expected error for whitespace-only passcode, got nil")
	}

	// Nothing should have been persisted.
	perms := config.LoadPermissions(dir)
	if perms.HasPasscode() {
		t.Error("a rejected empty passcode must not be persisted")
	}
}

// TestClearNodePasscodeLocksSurface confirms 'citadel passcode clear' removes
// a previously-set passcode so VerifyPasscode fails closed afterward.
func TestClearNodePasscodeLocksSurface(t *testing.T) {
	dir := t.TempDir()

	if _, err := setNodePasscode(dir, "5678"); err != nil {
		t.Fatalf("setNodePasscode: %v", err)
	}
	if !config.LoadPermissions(dir).HasPasscode() {
		t.Fatal("setup: passcode should be set before clearing")
	}

	perms, err := clearNodePasscode(dir)
	if err != nil {
		t.Fatalf("clearNodePasscode: %v", err)
	}
	if perms.HasPasscode() {
		t.Error("expected HasPasscode() false after clear")
	}

	reloaded := config.LoadPermissions(dir)
	if reloaded.HasPasscode() {
		t.Error("passcode clear was not persisted to disk")
	}
	if reloaded.VerifyPasscode("5678") {
		t.Error("cleared passcode must not still verify the old pin")
	}
}

// TestReadPasscodeFromReaderTrims confirms the piped-stdin path reads a
// single line and trims surrounding whitespace/newline.
func TestReadPasscodeFromReaderTrims(t *testing.T) {
	pin, err := readPasscodeFromReader(strings.NewReader("  9999  \nignored second line\n"))
	if err != nil {
		t.Fatalf("readPasscodeFromReader: %v", err)
	}
	if pin != "9999" {
		t.Errorf("pin = %q, want %q", pin, "9999")
	}
}

// TestReadPasscodeFromReaderEmptyInput confirms an empty stdin stream is
// reported as an error, not a silently-empty passcode.
func TestReadPasscodeFromReaderEmptyInput(t *testing.T) {
	if _, err := readPasscodeFromReader(strings.NewReader("")); err == nil {
		t.Error("expected error for empty stdin, got nil")
	}
}

// TestMatchPasscodeConfirmation covers both branches of the interactive
// double-entry confirmation check.
func TestMatchPasscodeConfirmation(t *testing.T) {
	if err := matchPasscodeConfirmation("1234", "1234"); err != nil {
		t.Errorf("matching entries should not error, got %v", err)
	}
	if err := matchPasscodeConfirmation("1234", "5678"); err == nil {
		t.Error("mismatched entries should error, got nil")
	}
}
