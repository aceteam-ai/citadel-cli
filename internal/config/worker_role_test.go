package config

import "testing"

func TestWorkerRoleResolution(t *testing.T) {
	for _, raw := range []WorkerRole{"", WorkerRoleFull, WorkerRoleControl} {
		role, err := ResolveWorkerRole(raw)
		if err != nil {
			t.Fatal(err)
		}
		if raw != WorkerRoleControl && role != WorkerRoleFull {
			t.Fatalf("legacy/full resolved as %q", role)
		}
		if (role.Report() == "control") != (raw == WorkerRoleControl) {
			t.Fatalf("unexpected report for %q: %q", raw, role.Report())
		}
	}
	if _, err := ResolveWorkerRole("typo"); err == nil {
		t.Fatal("unknown role must refuse, not use shared defaults")
	}
}
