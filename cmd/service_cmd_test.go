// cmd/service_cmd_test.go
package cmd

import (
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/service"
)

// TestCompetingManagedUnit_TableDriven pins citadel#882's fix: `citadel
// service install` must refuse when an already-ACTIVE citadel-managed unit is
// something other than the one about to be written, and must treat "no active
// unit" and "active unit already matches" as non-competing (safe to proceed).
func TestCompetingManagedUnit_TableDriven(t *testing.T) {
	cfg := service.ServiceConfig{UserMode: true}

	cases := []struct {
		name          string
		detect        func() (service.ManagedUnit, bool)
		wantCompeting bool
	}{
		{
			name:          "no active managed unit",
			detect:        func() (service.ManagedUnit, bool) { return service.ManagedUnit{}, false },
			wantCompeting: false,
		},
		{
			name: "active unit matches what's about to be installed (idempotent re-install)",
			detect: func() (service.ManagedUnit, bool) {
				return service.ManagedUnit{Name: service.ServiceName, UserMode: true}, true
			},
			wantCompeting: false,
		},
		{
			name: "fleet unit active (citadel-worker.service)",
			detect: func() (service.ManagedUnit, bool) {
				return service.ManagedUnit{Name: "citadel-worker", UserMode: false}, true
			},
			wantCompeting: true,
		},
		{
			name: "same name but different scope (system vs user) is still a duplicate",
			detect: func() (service.ManagedUnit, bool) {
				return service.ManagedUnit{Name: service.ServiceName, UserMode: false}, true
			},
			wantCompeting: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unit, competing := competingManagedUnit(cfg, tc.detect)
			if competing != tc.wantCompeting {
				t.Fatalf("competingManagedUnit() competing = %v, want %v", competing, tc.wantCompeting)
			}
			if competing && unit.Name == "" {
				t.Fatal("competingManagedUnit() reported competing=true but returned an empty ManagedUnit")
			}
		})
	}
}

// TestCompetingManagedUnitError_NamesUnitAndOffersForce checks the refusal
// message actually gives an operator something actionable: which unit is
// competing, an exact stop command, and the --force escape hatch.
func TestCompetingManagedUnitError_NamesUnitAndOffersForce(t *testing.T) {
	unit := service.ManagedUnit{Name: "citadel-worker", UserMode: false}
	err := competingManagedUnitError(unit)
	if err == nil {
		t.Fatal("expected a non-nil error")
	}
	msg := err.Error()

	for _, want := range []string{
		"citadel-worker.service (system service)", // unit.Description()
		"sudo systemctl stop citadel-worker",
		"--force",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("competingManagedUnitError() message missing %q; got:\n%s", want, msg)
		}
	}
}

// TestCompetingManagedUnitError_UserModeStopCommand checks the stop command
// uses the --user form for a user-scope unit.
func TestCompetingManagedUnitError_UserModeStopCommand(t *testing.T) {
	unit := service.ManagedUnit{Name: service.ServiceName, UserMode: true}
	msg := competingManagedUnitError(unit).Error()

	if !strings.Contains(msg, "systemctl --user stop "+service.ServiceName) {
		t.Errorf("expected a --user stop command; got:\n%s", msg)
	}
	if strings.Contains(msg, "sudo systemctl stop") {
		t.Errorf("did not expect a sudo/system stop command for a user-mode unit; got:\n%s", msg)
	}
}

// fakeInstallManager is a minimal service.Manager test double that only
// tracks Install calls, so tests never touch a real systemd/launchd/SCM.
type fakeInstallManager struct {
	installCalled bool
	installErr    error
}

func (f *fakeInstallManager) Install(service.ServiceConfig) error {
	f.installCalled = true
	return f.installErr
}
func (f *fakeInstallManager) Uninstall() error { return nil }
func (f *fakeInstallManager) Start() error     { return nil }
func (f *fakeInstallManager) Stop() error      { return nil }
func (f *fakeInstallManager) Status() (*service.ServiceStatus, error) {
	return &service.ServiceStatus{}, nil
}

// withRunSvcInstallSeams swaps runSvcInstall's activeManagedUnitFn,
// newServiceManagerFn, and svcForce for the duration of the test, restoring
// the originals on cleanup. This is the "thin seam" runSvcInstall exposes so
// its competing-unit refusal is testable end-to-end without shelling out to
// systemctl or writing real unit files.
func withRunSvcInstallSeams(t *testing.T, detect func() (service.ManagedUnit, bool), mgr *fakeInstallManager, force bool) {
	t.Helper()
	origDetect, origMgrFn, origForce := activeManagedUnitFn, newServiceManagerFn, svcForce
	activeManagedUnitFn = detect
	newServiceManagerFn = func() service.Manager { return mgr }
	svcForce = force
	t.Cleanup(func() {
		activeManagedUnitFn = origDetect
		newServiceManagerFn = origMgrFn
		svcForce = origForce
	})
}

// TestRunSvcInstall_RefusesOnCompetingUnit is the end-to-end regression guard
// for citadel#882: `citadel service install` must not call mgr.Install at all
// when a competing citadel-managed unit is already active.
func TestRunSvcInstall_RefusesOnCompetingUnit(t *testing.T) {
	mgr := &fakeInstallManager{}
	withRunSvcInstallSeams(t, func() (service.ManagedUnit, bool) {
		return service.ManagedUnit{Name: "citadel-worker", UserMode: false}, true
	}, mgr, false /* force */)

	err := runSvcInstall(nil, nil)
	if err == nil {
		t.Fatal("expected runSvcInstall to refuse, got nil error")
	}
	if !strings.Contains(err.Error(), "citadel-worker") {
		t.Errorf("expected the refusal to name the competing unit; got: %v", err)
	}
	if mgr.installCalled {
		t.Error("mgr.Install must NOT be called when a competing unit is refused")
	}
}

// TestRunSvcInstall_ForceProceedsDespiteCompetingUnit checks that --force
// (svcForce=true) skips the refusal and installs anyway.
func TestRunSvcInstall_ForceProceedsDespiteCompetingUnit(t *testing.T) {
	mgr := &fakeInstallManager{}
	withRunSvcInstallSeams(t, func() (service.ManagedUnit, bool) {
		return service.ManagedUnit{Name: "citadel-worker", UserMode: false}, true
	}, mgr, true /* force */)

	if err := runSvcInstall(nil, nil); err != nil {
		t.Fatalf("expected --force to proceed despite a competing unit, got error: %v", err)
	}
	if !mgr.installCalled {
		t.Error("expected mgr.Install to be called under --force")
	}
}

// TestRunSvcInstall_ProceedsWhenNoActiveUnit checks the common case: nothing
// active, install proceeds normally without --force.
func TestRunSvcInstall_ProceedsWhenNoActiveUnit(t *testing.T) {
	mgr := &fakeInstallManager{}
	withRunSvcInstallSeams(t, func() (service.ManagedUnit, bool) {
		return service.ManagedUnit{}, false
	}, mgr, false /* force */)

	if err := runSvcInstall(nil, nil); err != nil {
		t.Fatalf("expected install to proceed with no active unit, got error: %v", err)
	}
	if !mgr.installCalled {
		t.Error("expected mgr.Install to be called")
	}
}

// TestRunSvcInstall_ProceedsWhenActiveUnitMatches checks the idempotent
// re-install case: the active unit IS the one about to be installed.
func TestRunSvcInstall_ProceedsWhenActiveUnitMatches(t *testing.T) {
	mgr := &fakeInstallManager{}
	withRunSvcInstallSeams(t, func() (service.ManagedUnit, bool) {
		return service.ManagedUnit{Name: service.ServiceName, UserMode: resolveUserMode()}, true
	}, mgr, false /* force */)

	if err := runSvcInstall(nil, nil); err != nil {
		t.Fatalf("expected a matching active unit to be treated as idempotent, got error: %v", err)
	}
	if !mgr.installCalled {
		t.Error("expected mgr.Install to be called for an idempotent re-install")
	}
}
