// cmd/work_passcode_warning_test.go
package cmd

import (
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// TestSensitiveCapabilityPasscodeWarning_NoneEnabled confirms no warning is
// produced when console/desktop/files are all disabled, even with no
// passcode set (the default-deny posture for a fresh node).
func TestSensitiveCapabilityPasscodeWarning_NoneEnabled(t *testing.T) {
	perms := &config.Permissions{}
	if got := sensitiveCapabilityPasscodeWarning(perms); got != "" {
		t.Errorf("expected no warning, got %q", got)
	}
}

// TestSensitiveCapabilityPasscodeWarning_EnabledWithPasscode confirms no
// warning is produced when a sensitive surface is enabled AND a passcode is
// set (the working, unlocked configuration).
func TestSensitiveCapabilityPasscodeWarning_EnabledWithPasscode(t *testing.T) {
	perms := &config.Permissions{Console: true}
	if err := perms.SetPasscode("1234"); err != nil {
		t.Fatalf("SetPasscode: %v", err)
	}
	if got := sensitiveCapabilityPasscodeWarning(perms); got != "" {
		t.Errorf("expected no warning when a passcode is set, got %q", got)
	}
}

// TestSensitiveCapabilityPasscodeWarning_EnabledNoPasscode is the citadel#753
// case this warning exists for: a sensitive surface enabled with no passcode
// stays unreachable, and this is the only local, at-a-glance signal.
func TestSensitiveCapabilityPasscodeWarning_EnabledNoPasscode(t *testing.T) {
	perms := &config.Permissions{Console: true}
	got := sensitiveCapabilityPasscodeWarning(perms)
	if got == "" {
		t.Fatal("expected a warning when Console is enabled with no passcode set")
	}
	if !strings.Contains(got, "Console") {
		t.Errorf("warning should name the enabled surface, got %q", got)
	}
	if !strings.Contains(got, "citadel passcode set") {
		t.Errorf("warning should point at the fix, got %q", got)
	}
}

// TestSensitiveCapabilityPasscodeWarning_ListsAllEnabled confirms multiple
// enabled surfaces are all named, not just the first.
func TestSensitiveCapabilityPasscodeWarning_ListsAllEnabled(t *testing.T) {
	perms := &config.Permissions{Console: true, Desktop: true, Files: true}
	got := sensitiveCapabilityPasscodeWarning(perms)
	for _, want := range []string{"Console", "Desktop", "Files"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q should mention %q", got, want)
		}
	}
}

// TestSensitiveCapabilityPasscodeWarning_NilPermissions is a defensive check:
// a nil Permissions must never panic and must produce no warning (fail quiet,
// not fail loud on a nil-pointer crash at worker startup).
func TestSensitiveCapabilityPasscodeWarning_NilPermissions(t *testing.T) {
	if got := sensitiveCapabilityPasscodeWarning(nil); got != "" {
		t.Errorf("expected no warning for nil permissions, got %q", got)
	}
}
