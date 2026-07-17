// cmd/job_handlers_shell_test.go
package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// TestLegacyShellHandlerHonorsKillSwitch verifies the legacy Nexus/diagnostic
// job handler map wires the SHELL_COMMAND kill-switch from the persisted `shell`
// permission rather than registering an always-on handler. Before aceteam #6149
// this path used NewShellCommandHandler("") with no Disabled wiring, so it ran
// commands as root regardless of the node's permission (Phase 0 defuse).
func TestLegacyShellHandlerHonorsKillSwitch(t *testing.T) {
	h, ok := jobHandlers["SHELL_COMMAND"]
	if !ok {
		t.Fatal("SHELL_COMMAND handler not registered in legacy jobHandlers map")
	}
	shell, ok := h.(*jobs.ShellCommandHandler)
	if !ok {
		t.Fatalf("SHELL_COMMAND handler is %T, want *jobs.ShellCommandHandler", h)
	}

	// Disabled must track the persisted permission (default-deny), not be
	// hardcoded to the always-enabled zero value.
	wantDisabled := !config.LoadPermissions(platform.ConfigDir()).Shell
	if shell.Disabled != wantDisabled {
		t.Errorf("legacy SHELL_COMMAND handler Disabled=%v, want %v (from persisted shell permission)",
			shell.Disabled, wantDisabled)
	}
}

// TestLegacyShellHandlerDefaultDeny confirms that, absent an explicit opt-in
// permission, the shell kill-switch defaults to denying execution. This asserts
// the default-deny posture independent of the host's /etc/citadel config by
// loading permissions from an empty directory.
func TestLegacyShellHandlerDefaultDeny(t *testing.T) {
	perms := config.LoadPermissions(t.TempDir())
	if perms.Shell {
		t.Fatal("expected Shell to default to disabled (opt-in) when no config is present")
	}

	shell := jobs.NewShellCommandHandler("")
	shell.Disabled = !perms.Shell
	if !shell.Disabled {
		t.Error("shell handler should be Disabled by default (default-deny)")
	}

	// Opting in flips the switch.
	optedIn := &config.Permissions{Shell: true}
	shell.Disabled = !optedIn.Shell
	if shell.Disabled {
		t.Error("shell handler should be enabled once the shell permission is opted in")
	}
}
