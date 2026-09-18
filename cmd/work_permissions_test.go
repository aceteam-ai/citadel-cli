package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// TestPermissionsToHeartbeatHasPasscode pins that permissionsToHeartbeat
// (cmd/work.go) surfaces live passcode truth via config.Permissions.HasPasscode,
// not the hash itself (citadel #758). A remote controller (dashboard/MCP) needs
// this to distinguish "capability enabled but no passcode, remote access is
// blocked" from a healthy node, rather than inferring passcode state from its
// own dispatch history.
func TestPermissionsToHeartbeatHasPasscode(t *testing.T) {
	t.Run("no passcode set", func(t *testing.T) {
		p := config.DefaultPermissions()
		state := permissionsToHeartbeat(p)
		if state == nil {
			t.Fatal("expected non-nil PermissionState")
		}
		if state.HasPasscode {
			t.Error("expected HasPasscode=false when no passcode is set")
		}
	})

	t.Run("passcode set", func(t *testing.T) {
		p := config.DefaultPermissions()
		if err := p.SetPasscode("123456"); err != nil {
			t.Fatalf("SetPasscode: %v", err)
		}
		state := permissionsToHeartbeat(p)
		if state == nil {
			t.Fatal("expected non-nil PermissionState")
		}
		if !state.HasPasscode {
			t.Error("expected HasPasscode=true when a passcode is set")
		}
		// The heartbeat wire format must never carry the hash or plaintext.
		if state.HasPasscode != p.HasPasscode() {
			t.Errorf("HasPasscode=%v does not match config.Permissions.HasPasscode()=%v", state.HasPasscode, p.HasPasscode())
		}
	})

	t.Run("passcode cleared", func(t *testing.T) {
		p := config.DefaultPermissions()
		if err := p.SetPasscode("123456"); err != nil {
			t.Fatalf("SetPasscode: %v", err)
		}
		if err := p.SetPasscode(""); err != nil {
			t.Fatalf("SetPasscode(clear): %v", err)
		}
		state := permissionsToHeartbeat(p)
		if state.HasPasscode {
			t.Error("expected HasPasscode=false after clearing the passcode")
		}
	})

	t.Run("nil permissions", func(t *testing.T) {
		if state := permissionsToHeartbeat(nil); state != nil {
			t.Errorf("expected nil PermissionState for nil input, got %+v", state)
		}
	})
}

func TestPermissionsToHeartbeatWithAppliedKeepsRestartBoundary(t *testing.T) {
	applied := config.DefaultPermissions()
	desired := *applied
	desired.Console = true
	desired.Shell = true

	state := permissionsToHeartbeatWithApplied(&desired, applied)
	if state == nil || state.Applied == nil {
		t.Fatal("expected desired and applied permission state")
	}
	if !state.Console || !state.Shell {
		t.Fatalf("desired state = %+v, want console and shell enabled", state)
	}
	if state.Applied.Console {
		t.Fatal("console must remain at its startup snapshot until restart")
	}
	if !state.Applied.Shell {
		t.Fatal("shell should advance with its live per-job permission gate")
	}
}
