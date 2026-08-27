// cmd/update_test.go
package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/service"
)

// TestManagedServiceTargetFromManagerStatus_TableDriven pins the pure
// managed-vs-not-managed decision (citadel#454): a service.ServiceStatus only
// counts as a restart target when it is BOTH installed and currently running.
// An installed-but-stopped service has no live process to be split-brained
// with.
func TestManagedServiceTargetFromManagerStatus_TableDriven(t *testing.T) {
	cases := []struct {
		name      string
		status    *service.ServiceStatus
		wantFound bool
	}{
		{
			name:      "nil status",
			status:    nil,
			wantFound: false,
		},
		{
			name:      "not installed",
			status:    &service.ServiceStatus{Installed: false, Running: false},
			wantFound: false,
		},
		{
			name:      "installed but stopped",
			status:    &service.ServiceStatus{Installed: true, Running: false},
			wantFound: false,
		},
		{
			name:      "installed and running (managed)",
			status:    &service.ServiceStatus{Installed: true, Running: true, PID: 4242},
			wantFound: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &fakeManager{}
			target, found := managedServiceTargetFromManagerStatus(tc.status, mgr)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if !found {
				return
			}
			if target.RestartCmd == "" {
				t.Fatalf("expected a non-empty RestartCmd for a found target")
			}
			if target.Restart == nil {
				t.Fatalf("expected a non-nil Restart func for a found target")
			}
		})
	}
}

// TestManagedServiceTargetFromManagerStatus_RestartUsesStopThenStart verifies
// the Restart closure calls the Manager's own Stop then Start (the existing
// "citadel service stop && citadel service start" primitive), not a
// hand-rolled restart mechanism.
func TestManagedServiceTargetFromManagerStatus_RestartUsesStopThenStart(t *testing.T) {
	mgr := &fakeManager{}
	target, found := managedServiceTargetFromManagerStatus(&service.ServiceStatus{Installed: true, Running: true}, mgr)
	if !found {
		t.Fatalf("expected target to be found")
	}
	if err := target.Restart(); err != nil {
		t.Fatalf("Restart() returned error: %v", err)
	}
	if !mgr.stopped || !mgr.started {
		t.Fatalf("expected both Stop and Start to be called, got stopped=%v started=%v", mgr.stopped, mgr.started)
	}
	if mgr.stopOrder != 1 || mgr.startOrder != 2 {
		t.Fatalf("expected Stop before Start, got stopOrder=%d startOrder=%d", mgr.stopOrder, mgr.startOrder)
	}
}

// TestManagedServiceTargetFromManagerStatus_RestartPropagatesStopError checks
// that a Stop() failure is surfaced rather than silently proceeding to Start.
func TestManagedServiceTargetFromManagerStatus_RestartPropagatesStopError(t *testing.T) {
	mgr := &fakeManager{stopErr: errors.New("boom")}
	target, found := managedServiceTargetFromManagerStatus(&service.ServiceStatus{Installed: true, Running: true}, mgr)
	if !found {
		t.Fatalf("expected target to be found")
	}
	if err := target.Restart(); err == nil {
		t.Fatalf("expected Restart() to propagate the Stop error")
	}
	if mgr.started {
		t.Fatalf("Start should not be called after a Stop failure")
	}
}

// TestFormatManagedServiceWarning ensures the warning includes the restart
// command and the --restart hint, so an operator reading it has everything
// needed to act without further digging.
func TestFormatManagedServiceWarning(t *testing.T) {
	target := managedServiceTarget{
		Description: "citadel-worker.service (system service)",
		RestartCmd:  "sudo systemctl restart citadel-worker",
	}
	msg := formatManagedServiceWarning(target)

	if !strings.Contains(msg, "WARNING") {
		t.Fatalf("expected warning banner, got: %s", msg)
	}
	if !strings.Contains(msg, target.Description) {
		t.Fatalf("expected the service description in the warning, got: %s", msg)
	}
	if !strings.Contains(msg, target.RestartCmd) {
		t.Fatalf("expected the exact restart command in the warning, got: %s", msg)
	}
	if !strings.Contains(msg, "--restart") {
		t.Fatalf("expected a mention of the --restart flag, got: %s", msg)
	}
}

// TestRunManagedServiceGate_TableDriven is the warns-vs-restarts-vs-noop
// matrix for the manual `citadel update install` path (citadel#454 gap):
// not managed -> silent; managed + warn-only (default) -> prints the warning
// and never touches the service; managed + --restart -> calls Restart() and
// reports success or a non-zero exit on failure.
func TestRunManagedServiceGate_TableDriven(t *testing.T) {
	notFoundResolver := func() (managedServiceTarget, bool) { return managedServiceTarget{}, false }

	cases := []struct {
		name              string
		resolve           func(restartCalled *bool) func() (managedServiceTarget, bool)
		doRestart         bool
		wantExitCode      int
		wantOutSubstr     string
		wantErrSubstr     string
		wantRestartCalled bool
	}{
		{
			name:         "not managed: no warning, no restart, exit 0",
			resolve:      func(*bool) func() (managedServiceTarget, bool) { return notFoundResolver },
			doRestart:    false,
			wantExitCode: 0,
		},
		{
			name:         "not managed even with --restart requested: still a no-op",
			resolve:      func(*bool) func() (managedServiceTarget, bool) { return notFoundResolver },
			doRestart:    true,
			wantExitCode: 0,
		},
		{
			name: "managed, default (no --restart): warns loudly, does not restart",
			resolve: func(restartCalled *bool) func() (managedServiceTarget, bool) {
				return func() (managedServiceTarget, bool) {
					return managedServiceTarget{
						Description: "citadel.service (user service)",
						RestartCmd:  "systemctl --user restart citadel",
						Restart:     func() error { *restartCalled = true; return nil },
					}, true
				}
			},
			doRestart:         false,
			wantExitCode:      0,
			wantOutSubstr:     "WARNING",
			wantRestartCalled: false,
		},
		{
			name: "managed + --restart: restarts and reports success",
			resolve: func(restartCalled *bool) func() (managedServiceTarget, bool) {
				return func() (managedServiceTarget, bool) {
					return managedServiceTarget{
						Description: "citadel-worker.service (system service)",
						RestartCmd:  "sudo systemctl restart citadel-worker",
						Restart:     func() error { *restartCalled = true; return nil },
					}, true
				}
			},
			doRestart:         true,
			wantExitCode:      0,
			wantOutSubstr:     "Service restarted",
			wantRestartCalled: true,
		},
		{
			name: "managed + --restart, restart fails: non-zero exit, error reported",
			resolve: func(restartCalled *bool) func() (managedServiceTarget, bool) {
				return func() (managedServiceTarget, bool) {
					return managedServiceTarget{
						Description: "citadel.service (system service)",
						RestartCmd:  "sudo systemctl restart citadel",
						Restart:     func() error { *restartCalled = true; return errors.New("permission denied") },
					}, true
				}
			},
			doRestart:         true,
			wantExitCode:      1,
			wantOutSubstr:     "Restart it manually",
			wantErrSubstr:     "permission denied",
			wantRestartCalled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			var restartCalled bool
			code := runManagedServiceGate(tc.doRestart, tc.resolve(&restartCalled), &out, &errOut)
			if code != tc.wantExitCode {
				t.Fatalf("exit code = %d, want %d (stdout=%q stderr=%q)", code, tc.wantExitCode, out.String(), errOut.String())
			}
			if restartCalled != tc.wantRestartCalled {
				t.Fatalf("restartCalled = %v, want %v", restartCalled, tc.wantRestartCalled)
			}
			if tc.wantOutSubstr != "" && !strings.Contains(out.String(), tc.wantOutSubstr) {
				t.Fatalf("stdout %q does not contain %q", out.String(), tc.wantOutSubstr)
			}
			if tc.wantErrSubstr != "" && !strings.Contains(errOut.String(), tc.wantErrSubstr) {
				t.Fatalf("stderr %q does not contain %q", errOut.String(), tc.wantErrSubstr)
			}
		})
	}
}

// fakeManager is a minimal service.Manager test double that records Stop/Start
// call order without touching systemctl/launchctl/sc.
type fakeManager struct {
	stopped, started      bool
	stopOrder, startOrder int
	callSeq               int
	stopErr, startErr     error
}

func (f *fakeManager) Install(service.ServiceConfig) error { return nil }
func (f *fakeManager) Uninstall() error                    { return nil }

func (f *fakeManager) Start() error {
	f.callSeq++
	f.started = true
	f.startOrder = f.callSeq
	return f.startErr
}

func (f *fakeManager) Stop() error {
	f.callSeq++
	f.stopped = true
	f.stopOrder = f.callSeq
	return f.stopErr
}

func (f *fakeManager) Status() (*service.ServiceStatus, error) {
	return &service.ServiceStatus{Installed: true, Running: true}, nil
}
