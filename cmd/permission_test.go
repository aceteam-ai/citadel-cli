package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

func TestSetShellPermissionPreservesOtherPolicy(t *testing.T) {
	dir := t.TempDir()
	perms := config.DefaultPermissions()
	perms.Console = true
	if err := perms.SetPasscode("2468"); err != nil {
		t.Fatalf("set passcode: %v", err)
	}
	if err := config.SavePermissions(dir, perms); err != nil {
		t.Fatalf("save initial permissions: %v", err)
	}

	updated, err := setShellPermission(dir, true)
	if err != nil {
		t.Fatalf("enable shell: %v", err)
	}
	if !updated.Shell || !updated.Console || !updated.VerifyPasscode("2468") {
		t.Fatalf("updated permissions lost existing policy: %+v", updated)
	}

	updated, err = setShellPermission(dir, false)
	if err != nil {
		t.Fatalf("disable shell: %v", err)
	}
	if updated.Shell {
		t.Fatal("local shell kill switch did not persist")
	}
	if !updated.Console || !updated.VerifyPasscode("2468") {
		t.Fatalf("disable changed unrelated policy: %+v", updated)
	}
}
