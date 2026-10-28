// cmd/update_test.go
package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/service"
	"github.com/aceteam-ai/citadel-cli/internal/update"
)

func TestDownloadAndApplyManualCleansPrivateAttemptBeforeReturn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalNew := newUpdateAttemptFn
	originalDownload := downloadReleaseFn
	originalApply := applyReleaseFn
	t.Cleanup(func() {
		newUpdateAttemptFn = originalNew
		downloadReleaseFn = originalDownload
		applyReleaseFn = originalApply
	})
	var attemptDir string
	newUpdateAttemptFn = func() (*update.UpdateAttempt, error) {
		attempt, err := update.NewUpdateAttempt()
		if attempt != nil {
			attemptDir = attempt.Dir
		}
		return attempt, err
	}
	downloadReleaseFn = func(*update.Client, *update.Release, string) error { return nil }
	applyReleaseFn = func(context.Context, string, string, string, update.ApplyIntent) (update.ApplyResult, error) {
		return update.ApplyResult{Disposition: update.AlreadyCurrent, InstalledAfter: "v2.0.0"}, nil
	}
	client := update.NewClient("v1.0.0")
	result, err := downloadAndApplyManual(client, &update.Release{TagName: "v2.0.0"}, nil)
	if err != nil || result.Disposition != update.AlreadyCurrent {
		t.Fatalf("result = %#v, %v", result, err)
	}
	if attemptDir == "" {
		t.Fatal("private attempt was not allocated")
	}
	if _, err := os.Lstat(attemptDir); !os.IsNotExist(err) {
		t.Fatalf("attempt still exists after returning helper: %s: %v", filepath.Base(attemptDir), err)
	}
}

func TestManualUpdatePermissionPreflightBeforeDownloadOrAttempt(t *testing.T) {
	withAutoUpdateEnv(t, "v2.202.0")
	originalCapability, originalNew, originalDownload := manualUpdateCapabilityFn, newUpdateAttemptFn, downloadReleaseFn
	t.Cleanup(func() {
		manualUpdateCapabilityFn, newUpdateAttemptFn, downloadReleaseFn = originalCapability, originalNew, originalDownload
	})
	capability := update.SelfUpdateCapability{State: update.SelfUpdatePrivileged, Directory: "/fixture/privileged", BinaryOwner: "root", WorkerUser: "fixture-worker"}
	manualUpdateCapabilityFn = func() update.SelfUpdateCapability { return capability }
	calls := 0
	newUpdateAttemptFn = func() (*update.UpdateAttempt, error) { calls++; return nil, errors.New("unexpected fixture attempt") }
	downloadReleaseFn = func(*update.Client, *update.Release, string) error { calls++; return nil }
	_, err := downloadAndApplyManual(update.NewClient("v2.202.0"), &update.Release{TagName: "v2.203.0"}, nil)
	if err == nil || calls != 0 || !strings.Contains(err.Error(), capability.BinaryOwner) || !strings.Contains(err.Error(), capability.WorkerUser) || !strings.Contains(err.Error(), "before download") {
		t.Fatalf("manual preflight downloaded or omitted remediation: calls=%d err=%v", calls, err)
	}
}

func TestDownloadAndApplyManualCleansPrivateAttemptOnErrors(t *testing.T) {
	for _, phase := range []string{"download", "apply"} {
		t.Run(phase, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			originalNew := newUpdateAttemptFn
			originalDownload := downloadReleaseFn
			originalApply := applyReleaseFn
			t.Cleanup(func() {
				newUpdateAttemptFn = originalNew
				downloadReleaseFn = originalDownload
				applyReleaseFn = originalApply
			})
			var attemptDir string
			newUpdateAttemptFn = func() (*update.UpdateAttempt, error) {
				attempt, err := update.NewUpdateAttempt()
				if attempt != nil {
					attemptDir = attempt.Dir
				}
				return attempt, err
			}
			downloadReleaseFn = func(*update.Client, *update.Release, string) error {
				if phase == "download" {
					return errors.New("download failed")
				}
				return nil
			}
			applyReleaseFn = func(context.Context, string, string, string, update.ApplyIntent) (update.ApplyResult, error) {
				return update.ApplyResult{}, errors.New("apply failed")
			}
			_, err := downloadAndApplyManual(update.NewClient("v1.0.0"), &update.Release{TagName: "v2.0.0"}, nil)
			if err == nil {
				t.Fatal("error path returned nil")
			}
			if _, err := os.Lstat(attemptDir); !os.IsNotExist(err) {
				t.Fatalf("attempt still exists after %s error: %v", phase, err)
			}
		})
	}
}

func TestFinishManualNoOpRefreshesManagedUnitsWithoutUpdateState(t *testing.T) {
	original := rematerializeManagedUnitsFn
	t.Cleanup(func() { rematerializeManagedUnitsFn = original })
	called := 0
	rematerializeManagedUnitsFn = func(func(string, ...any)) ([]string, error) {
		called++
		return nil, nil
	}
	var out strings.Builder
	if !finishManualNoOp(update.ApplyResult{Disposition: update.Superseded, InstalledAfter: "v2.0.0"}, &out, false, func() { t.Fatal("no restart was requested") }) {
		t.Fatal("superseded manual apply was not handled as no-op")
	}
	if called != 1 {
		t.Fatalf("managed-unit refresh calls = %d, want 1", called)
	}
	if finishManualNoOp(update.ApplyResult{Disposition: update.Applied}, &out, false, func() { t.Fatal("no restart was requested") }) {
		t.Fatal("applied update handled as no-op")
	}
}

func TestFinishManualNoOpHonorsExplicitRestart(t *testing.T) {
	original := rematerializeManagedUnitsFn
	t.Cleanup(func() { rematerializeManagedUnitsFn = original })
	for _, disposition := range []update.ApplyDisposition{update.AlreadyCurrent, update.Superseded, update.Applied} {
		for _, requested := range []bool{false, true} {
			var order []string
			rematerializeManagedUnitsFn = func(func(string, ...any)) ([]string, error) { order = append(order, "refresh"); return nil, nil }
			var out bytes.Buffer
			handled := finishManualNoOp(update.ApplyResult{Disposition: disposition}, &out, requested, func() { order = append(order, "restart") })
			if disposition == update.Applied {
				if handled || len(order) != 0 {
					t.Fatalf("applied result was handled twice: %v %v", handled, order)
				}
			} else {
				want := []string{"refresh"}
				if requested {
					want = append(want, "restart")
				}
				if !handled || !reflect.DeepEqual(order, want) {
					t.Fatalf("explicit no-swap restart not honored: result=%v requested=%t order=%v", disposition, requested, order)
				}
			}
		}
	}
}

func TestManagedWorkerRestartRevalidatesAfterDrain(t *testing.T) {
	worker := service.ManagedWorker{Unit: service.ManagedUnit{Name: "citadel-work"}, Executable: "/fixture/citadel", User: "fixture-user"}
	for _, tc := range []struct {
		name    string
		fresh   []service.ManagedWorker
		err     error
		success bool
	}{
		{name: "same worker", fresh: []service.ManagedWorker{worker}, success: true},
		{name: "worker disappeared"},
		{name: "ambiguous", fresh: []service.ManagedWorker{worker, worker}},
		{name: "different unit", fresh: []service.ManagedWorker{{Unit: service.ManagedUnit{Name: "other"}, Executable: worker.Executable, User: worker.User}}},
		{name: "different executable", fresh: []service.ManagedWorker{{Unit: worker.Unit, Executable: "/foreign/citadel", User: worker.User}}},
		{name: "different user", fresh: []service.ManagedWorker{{Unit: worker.Unit, Executable: worker.Executable, User: "other-user"}}},
		{name: "unavailable", err: errors.New("synthetic lookup failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drained, restarted := false, false
			target, found := managedServiceTargetFromWorkers([]service.ManagedWorker{worker}, func() ([]service.ManagedWorker, error) {
				if !drained {
					t.Fatal("revalidation ran before draining")
				}
				return tc.fresh, tc.err
			}, func(got service.ManagedUnit) error {
				restarted = true
				if got != worker.Unit {
					t.Fatal("restarted another unit")
				}
				return nil
			})
			if !found {
				t.Fatal("verified custom worker was not offered as target")
			}
			var out, errOut bytes.Buffer
			code := runManagedServiceGate(true, func() (managedServiceTarget, bool) { return target, true }, func(io.Writer) { drained = true }, &out, &errOut)
			if restarted != tc.success || (code == 0) != tc.success {
				t.Fatalf("stale/ambiguous worker restart result: code=%d restarted=%t error=%s", code, restarted, &errOut)
			}
		})
	}
}

func TestManagedWorkerRestartRequiresOneInitialMatch(t *testing.T) {
	worker := service.ManagedWorker{Unit: service.ManagedUnit{Name: "citadel-work"}, Executable: "/fixture/citadel"}
	for _, workers := range [][]service.ManagedWorker{nil, {worker, worker}} {
		if _, found := managedServiceTargetFromWorkers(workers, func() ([]service.ManagedWorker, error) { t.Fatal("unverified target was refreshed"); return nil, nil }, func(service.ManagedUnit) error { t.Fatal("unverified target was restarted"); return nil }); found {
			t.Fatal("non-unique restart target was accepted")
		}
	}
}

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
			name:          "restart requested without verified unit: fails loudly",
			resolve:       func(*bool) func() (managedServiceTarget, bool) { return notFoundResolver },
			doRestart:     true,
			wantExitCode:  1,
			wantErrSubstr: "no service was restarted",
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
			code := runManagedServiceGate(tc.doRestart, tc.resolve(&restartCalled), nil, &out, &errOut)
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

// TestRunManagedServiceGate_DrainCalledOnlyOnRestartPath pins WHEN drain runs:
// only on the doRestart==true, found==true path, and always before
// target.Restart() -- never on warn-only, never when nothing is managed, and
// (citadel#887's whole point) never blocking the not-managed/warn-only cases
// that never restart at all.
func TestRunManagedServiceGate_DrainCalledOnlyOnRestartPath(t *testing.T) {
	notFoundResolver := func() (managedServiceTarget, bool) { return managedServiceTarget{}, false }
	foundResolver := func() (managedServiceTarget, bool) {
		return managedServiceTarget{
			Description: "citadel.service (system service)",
			RestartCmd:  "systemctl restart citadel",
			Restart:     func() error { return nil },
		}, true
	}

	cases := []struct {
		name           string
		resolve        func() (managedServiceTarget, bool)
		doRestart      bool
		wantDrainOrder string // "" = not called, else must precede "restart" in the recorded order
	}{
		{name: "not managed, no --restart: drain not called", resolve: notFoundResolver, doRestart: false},
		{name: "not managed, --restart: drain not called", resolve: notFoundResolver, doRestart: true},
		{name: "managed, no --restart (warn-only): drain not called", resolve: foundResolver, doRestart: false},
		{name: "managed, --restart: drain called before Restart()", resolve: foundResolver, doRestart: true, wantDrainOrder: "drain-then-restart"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			resolve := tc.resolve
			if tc.wantDrainOrder != "" {
				resolve = func() (managedServiceTarget, bool) {
					return managedServiceTarget{
						Description: "citadel.service (system service)",
						RestartCmd:  "systemctl restart citadel",
						Restart:     func() error { order = append(order, "restart"); return nil },
					}, true
				}
			}
			drainCalled := false
			drain := func(io.Writer) { drainCalled = true; order = append(order, "drain") }

			var out, errOut bytes.Buffer
			runManagedServiceGate(tc.doRestart, resolve, drain, &out, &errOut)

			wantDrainCalled := tc.wantDrainOrder != ""
			if drainCalled != wantDrainCalled {
				t.Fatalf("drainCalled = %v, want %v", drainCalled, wantDrainCalled)
			}
			if tc.wantDrainOrder != "" {
				if len(order) != 2 || order[0] != "drain" || order[1] != "restart" {
					t.Fatalf("call order = %v, want [drain restart]", order)
				}
			}
		})
	}
}

// TestRunManagedServiceGate_NilDrainSkipsWait confirms a nil drain (used by
// the pre-#887 table-driven test above, and any future caller that doesn't
// want the wait) is a byte-identical no-op for the drain step -- restart
// still proceeds immediately.
func TestRunManagedServiceGate_NilDrainSkipsWait(t *testing.T) {
	restartCalled := false
	resolve := func() (managedServiceTarget, bool) {
		return managedServiceTarget{
			Description: "citadel.service (system service)",
			RestartCmd:  "systemctl restart citadel",
			Restart:     func() error { restartCalled = true; return nil },
		}, true
	}
	var out, errOut bytes.Buffer
	code := runManagedServiceGate(true, resolve, nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !restartCalled {
		t.Fatalf("expected Restart() to be called with a nil drain")
	}
}

// TestDrainManagedServiceBeforeRestart_TableDriven pins the pure wait-loop
// decision (citadel#887) against fake fetchers, so the timing/looping logic
// is verified without a real HTTP server or worker.
func TestDrainManagedServiceBeforeRestart_TableDriven(t *testing.T) {
	t.Run("in_flight drops to zero before timeout: returns promptly, no timeout message", func(t *testing.T) {
		calls := 0
		fetch := func() (int64, bool) {
			calls++
			if calls < 3 {
				return 2, true
			}
			return 0, true
		}
		var out bytes.Buffer
		start := time.Now()
		drainManagedServiceBeforeRestart(fetch, time.Minute, time.Millisecond, &out)
		elapsed := time.Since(start)

		if calls < 3 {
			t.Fatalf("expected at least 3 fetch calls before in_flight reached 0, got %d", calls)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("drain took %s, expected it to return promptly once in_flight hit 0", elapsed)
		}
		if strings.Contains(out.String(), "Timed out") {
			t.Fatalf("did not expect a timeout message, got: %s", out.String())
		}
	})

	t.Run("in_flight never reaches zero: restarts anyway after timeout, with a clear message", func(t *testing.T) {
		fetch := func() (int64, bool) { return 1, true }
		var out bytes.Buffer
		start := time.Now()
		drainManagedServiceBeforeRestart(fetch, 20*time.Millisecond, 5*time.Millisecond, &out)
		elapsed := time.Since(start)

		if elapsed < 20*time.Millisecond {
			t.Fatalf("drain returned before its own timeout elapsed (%s)", elapsed)
		}
		if !strings.Contains(out.String(), "Timed out") {
			t.Fatalf("expected a timeout message, got: %s", out.String())
		}
		if !strings.Contains(out.String(), "restarting anyway") {
			t.Fatalf("expected the message to say it is restarting anyway, got: %s", out.String())
		}
	})

	t.Run("fetch cannot determine in_flight (unreachable/404/etc): returns immediately, never blocks", func(t *testing.T) {
		calls := 0
		fetch := func() (int64, bool) { calls++; return 0, false }
		var out bytes.Buffer
		start := time.Now()
		// A long timeout: if this ever loops/sleeps on an unreadable signal, the
		// test would time out the whole suite instead of failing fast.
		drainManagedServiceBeforeRestart(fetch, time.Hour, time.Hour, &out)
		elapsed := time.Since(start)

		if calls != 1 {
			t.Fatalf("expected exactly one fetch call before giving up, got %d", calls)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("drain took %s, expected it to return immediately on an unreadable signal", elapsed)
		}
		if !strings.Contains(out.String(), "restarting without waiting") {
			t.Fatalf("expected a message explaining the skip, got: %s", out.String())
		}
	})

	t.Run("in_flight already zero on first read: no waiting message printed", func(t *testing.T) {
		fetch := func() (int64, bool) { return 0, true }
		var out bytes.Buffer
		drainManagedServiceBeforeRestart(fetch, time.Minute, time.Millisecond, &out)
		if out.Len() != 0 {
			t.Fatalf("expected no output when already at zero in-flight, got: %s", out.String())
		}
	})
}

// TestFetchWorkerInFlight_TableDriven exercises the real HTTP GET against an
// httptest.Server, covering the response shapes GET /worker can actually
// produce (citadel#735's route): a normal in_flight payload, a 404 (an older
// worker predating the route), a malformed body, and an unreachable server
// (closed connection) -- the last two must degrade to ok=false, never to a
// misleading in_flight=0.
func TestFetchWorkerInFlight_TableDriven(t *testing.T) {
	t.Run("normal payload: extracts in_flight", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/worker" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"worker":{"consuming":true,"in_flight":3}}`))
		}))
		defer srv.Close()

		inFlight, ok := fetchWorkerInFlight(srv.URL)
		if !ok {
			t.Fatalf("expected ok=true")
		}
		if inFlight != 3 {
			t.Fatalf("in_flight = %d, want 3", inFlight)
		}
	})

	t.Run("worker block absent: ok=false, not a misleading zero", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}))
		defer srv.Close()

		_, ok := fetchWorkerInFlight(srv.URL)
		if ok {
			t.Fatalf("expected ok=false when the worker block is absent")
		}
	})

	t.Run("404 (older worker predating /worker): ok=false", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		defer srv.Close()

		_, ok := fetchWorkerInFlight(srv.URL)
		if ok {
			t.Fatalf("expected ok=false on a 404")
		}
	})

	t.Run("malformed body: ok=false", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()

		_, ok := fetchWorkerInFlight(srv.URL)
		if ok {
			t.Fatalf("expected ok=false on a malformed body")
		}
	})

	t.Run("unreachable server: ok=false", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL
		srv.Close() // now nothing is listening

		_, ok := fetchWorkerInFlight(url)
		if ok {
			t.Fatalf("expected ok=false when nothing is listening")
		}
	})
}

// TestRunManagedServiceGate_DrainAgainstRealHTTPServer is the end-to-end
// wiring test: an httptest.Server standing in for the running worker's status
// server, driving the exact drainManagedServiceBeforeRestart closure
// warnOrRestartManagedService constructs (minus the resolveStatusPort() hop),
// through the full runManagedServiceGate path. Confirms in_flight dropping to
// zero over real HTTP calls unblocks the restart.
func TestRunManagedServiceGate_DrainAgainstRealHTTPServer(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		inFlight := 0
		if n < 3 {
			inFlight = 1
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"worker":{"in_flight":%d}}`, inFlight)
	}))
	defer srv.Close()

	drain := func(out io.Writer) {
		drainManagedServiceBeforeRestart(func() (int64, bool) { return fetchWorkerInFlight(srv.URL) },
			time.Minute, time.Millisecond, out)
	}
	restartCalled := false
	resolve := func() (managedServiceTarget, bool) {
		return managedServiceTarget{
			Description: "citadel.service (system service)",
			RestartCmd:  "systemctl restart citadel",
			Restart:     func() error { restartCalled = true; return nil },
		}, true
	}

	var out, errOut bytes.Buffer
	code := runManagedServiceGate(true, resolve, drain, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout=%q stderr=%q)", code, out.String(), errOut.String())
	}
	if !restartCalled {
		t.Fatalf("expected Restart() to be called once in_flight reached 0")
	}
	if atomic.LoadInt32(&calls) < 3 {
		t.Fatalf("expected at least 3 polls against the real server, got %d", calls)
	}
}

// TestRefreshManagedServiceUnits_TableDriven pins the shared unit-refresh helper
// (citadel-cli#1266) that BOTH branches of `citadel update install` (post-install
// and the already-latest early return) and `citadel service refresh-unit` now
// call. It swaps the rematerializeManagedUnitsFn seam with a recorder, so no
// real systemd unit is touched and systemctl is never invoked.
func TestRefreshManagedServiceUnits_TableDriven(t *testing.T) {
	orig := rematerializeManagedUnitsFn
	t.Cleanup(func() { rematerializeManagedUnitsFn = orig })

	cases := []struct {
		name          string
		fn            func(logf func(string, ...any)) ([]string, error)
		wantRewritten int
		wantReported  bool
		wantOutSubstr []string
		wantOutAbsent []string
	}{
		{
			name: "drift rewritten: summary + next-restart line",
			fn: func(logf func(string, ...any)) ([]string, error) {
				logf("unit-refresh: %s: applied #444 restart-storm hardening", "/etc/systemd/system/citadel-worker.service")
				return []string{"/etc/systemd/system/citadel-worker.service"}, nil
			},
			wantRewritten: 1,
			wantReported:  true,
			wantOutSubstr: []string{
				"Refreshed managed service unit(s): /etc/systemd/system/citadel-worker.service",
				"applies on the next service restart",
				"   - unit-refresh:",
			},
		},
		{
			name: "no drift: clean no-op, no output",
			fn: func(logf func(string, ...any)) ([]string, error) {
				return nil, nil // nothing rewritten, logf never called
			},
			wantRewritten: 0,
			wantReported:  false,
			wantOutAbsent: []string{"Refreshed", "Warning", "   - "},
		},
		{
			name: "needs root (non-root system unit): remediation reported, nothing rewritten",
			fn: func(logf func(string, ...any)) ([]string, error) {
				logf("unit-refresh: %s: needs the #444 restart-storm hardening but rewriting a system unit requires root; re-run as root: %s",
					"/etc/systemd/system/citadel-worker.service", `sudo "/home/jason/.local/bin/citadel" service refresh-unit`)
				return nil, nil
			},
			wantRewritten: 0,
			wantReported:  true,
			wantOutSubstr: []string{"requires root", "service refresh-unit"},
			wantOutAbsent: []string{"Refreshed", "sudo citadel "},
		},
		{
			name: "systemic error: warning, nothing rewritten",
			fn: func(logf func(string, ...any)) ([]string, error) {
				return nil, errors.New("sweep boom")
			},
			wantRewritten: 0,
			wantReported:  true,
			wantOutSubstr: []string{"Warning: could not refresh managed service unit(s): sweep boom"},
			wantOutAbsent: []string{"Refreshed"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rematerializeManagedUnitsFn = tc.fn
			var out bytes.Buffer
			rewritten, reported := refreshManagedServiceUnits(&out)
			if rewritten != tc.wantRewritten {
				t.Fatalf("rewritten = %d, want %d", rewritten, tc.wantRewritten)
			}
			if reported != tc.wantReported {
				t.Fatalf("reported = %v, want %v (out=%q)", reported, tc.wantReported, out.String())
			}
			for _, want := range tc.wantOutSubstr {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output %q missing %q", out.String(), want)
				}
			}
			for _, absent := range tc.wantOutAbsent {
				if strings.Contains(out.String(), absent) {
					t.Errorf("output %q should not contain %q", out.String(), absent)
				}
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

func TestManagedServiceDiscoveryFailuresAreNotNoWorker(t *testing.T) {
	for _, phase := range []string{"executable", "discovery", "multiple workers"} {
		for _, requested := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restart=%t", phase, requested), func(t *testing.T) {
				cause := errors.New("fixture loaded unit is stale")
				target, found := resolveManagedWorkersTarget(func() (string, error) {
					if phase == "executable" {
						return "", cause
					}
					return "/fixture/citadel", nil
				}, func(path string) ([]service.ManagedWorker, error) {
					if phase == "executable" {
						t.Fatal("discovery ran after executable failure")
					}
					if path != "/fixture/citadel" {
						t.Fatal("wrong executable")
					}
					if phase == "discovery" {
						return nil, cause
					}
					return []service.ManagedWorker{{}, {}}, nil
				})
				if found || target.DiscoveryErr == nil {
					t.Fatal("failure became a successful no-worker observation")
				}
				var out, diagnostics bytes.Buffer
				code := runManagedServiceGate(requested, func() (managedServiceTarget, bool) { return target, found }, func(io.Writer) { t.Fatal("unverified target was drained") }, &out, &diagnostics)
				if (code != 0) != requested || !strings.Contains(diagnostics.String(), "discovery failed:") || !strings.Contains(diagnostics.String(), target.DiscoveryErr.Error()) {
					t.Fatalf("discovery cause or exit lost: code=%d output=%q diagnostics=%q", code, out.String(), diagnostics.String())
				}
				if !requested && !strings.Contains(diagnostics.String(), "may still be on the old binary") {
					t.Fatal("applied swap lost its stale-worker warning")
				}
			})
		}
	}
	var out, diagnostics bytes.Buffer
	target, found := resolveManagedWorkersTarget(func() (string, error) { return "/fixture/citadel", nil }, func(string) ([]service.ManagedWorker, error) { return nil, nil })
	if found || target.DiscoveryErr != nil || runManagedServiceGate(false, func() (managedServiceTarget, bool) { return target, found }, nil, &out, &diagnostics) != 0 || out.Len() != 0 || diagnostics.Len() != 0 {
		t.Fatal("verified worker absence made unsolicited noise")
	}
}
