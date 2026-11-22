package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/localaudit"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/service"
)

func TestBackgroundArgvStripsModeFlags(t *testing.T) {
	launcher := []string{"--debug", "work", "--background", "--no-gateway", "--status-port", "8080"}
	sup := supervisorArgv(launcher)
	wantSup := []string{"--debug", "work", "--no-gateway", "--status-port", "8080", "--supervise"}
	if !reflect.DeepEqual(sup, wantSup) {
		t.Fatalf("supervisorArgv = %v, want %v", sup, wantSup)
	}
	worker := supervisedWorkerArgv(sup, 4242)
	wantWorker := []string{"--debug", "work", "--no-gateway", "--status-port", "8080", "--supervisor-pid=4242"}
	if !reflect.DeepEqual(worker, wantWorker) {
		t.Fatalf("supervisedWorkerArgv = %v, want %v", worker, wantWorker)
	}
	// Every spelling of the mode flags is removed, so no forwarded command line
	// can re-enter a background mode.
	got := stripWorkModeFlags([]string{"work", "--background=true", "--supervise=true", "--supervisor-pid", "7", "--supervisor-pid=8", "--terminal-port", "7860"})
	if want := []string{"work", "--terminal-port", "7860"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stripWorkModeFlags = %v, want %v", got, want)
	}
}

// withSupervisorSeams sets the supervised-worker inputs for one test.
func withSupervisorSeams(t *testing.T, flagPID, ppid int, held bool, holder int) {
	t.Helper()
	oldPID, oldParent, oldHolder := workSupervisorPID, parentPID, supervisorLockHolder
	t.Cleanup(func() { workSupervisorPID, parentPID, supervisorLockHolder = oldPID, oldParent, oldHolder })
	workSupervisorPID = flagPID
	parentPID = func() int { return ppid }
	supervisorLockHolder = func() (bool, int) { return held, holder }
}

func TestSupervisedByUserSupervisorRequiresLiveVerifiedParent(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("CITADEL_SERVICE", "")
	cases := []struct {
		name                     string
		flagPID, ppid, holderPID int
		held                     bool
		want                     bool
	}{
		{"no flag", 0, 100, 100, true, false},
		{"parent is not the named supervisor", 100, 200, 100, true, false},
		{"supervisor lock not held", 100, 100, 0, false, false},
		{"lock held by another process", 100, 100, 300, true, false},
		{"verified supervisor", 100, 100, 100, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withSupervisorSeams(t, tc.flagPID, tc.ppid, tc.held, tc.holderPID)
			if got := supervisedByUserSupervisor(); got != tc.want {
				t.Fatalf("supervisedByUserSupervisor = %v, want %v", got, tc.want)
			}
			if got := managedByServiceManager(); got != tc.want {
				t.Fatalf("managedByServiceManager = %v, want %v", got, tc.want)
			}
			if got := agentUpdateIsService(); got != tc.want {
				t.Fatalf("agentUpdateIsService = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAgentUpdateIsServiceKeepsServiceEnv(t *testing.T) {
	withSupervisorSeams(t, 0, 0, false, 0)
	t.Setenv("CITADEL_SERVICE", "true")
	if !agentUpdateIsService() {
		t.Fatal("CITADEL_SERVICE=true no longer counts as a service")
	}
}

func TestWatchStopFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), service.SupervisorStopFileName)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go watchStopFile(ctx, path, 5*time.Millisecond, func() { close(stopped) })
	select {
	case <-stopped:
		t.Fatal("stopped before any request")
	case <-time.After(30 * time.Millisecond):
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop request not noticed")
	}
}

func TestOpenBackgroundLogRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", backgroundLogName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), backgroundLogMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openBackgroundLog(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatalf("log was not rotated: %v %v", info, err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated log missing: %v", err)
	}
}

func withSupervisorHeld(t *testing.T, held func(string) (bool, int)) {
	t.Helper()
	old, oldPoll := userSupervisorHeld, userWorkerPoll
	t.Cleanup(func() { userSupervisorHeld, userWorkerPoll = old, oldPoll })
	userSupervisorHeld = held
	userWorkerPoll = time.Millisecond
}

func TestStopUserWorker(t *testing.T) {
	t.Run("nothing running", func(t *testing.T) {
		dir := t.TempDir()
		withSupervisorHeld(t, func(string) (bool, int) { return false, 0 })
		var out bytes.Buffer
		if err := stopUserWorker(dir, time.Second, &out); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(service.SupervisorStopPath(dir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stop file written with no supervisor running: %v", err)
		}
	})
	t.Run("supervisor exits after the request", func(t *testing.T) {
		dir := t.TempDir()
		withSupervisorHeld(t, func(lock string) (bool, int) {
			if lock != service.SupervisorLockPath(dir) {
				t.Errorf("probed %s", lock)
			}
			_, err := os.Stat(service.SupervisorStopPath(dir))
			return err != nil, 99 // held until the stop file exists
		})
		var out bytes.Buffer
		if err := stopUserWorker(dir, 5*time.Second, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Stopped.") {
			t.Fatalf("output = %q", out.String())
		}
	})
	t.Run("supervisor never exits", func(t *testing.T) {
		dir := t.TempDir()
		withSupervisorHeld(t, func(string) (bool, int) { return true, 99 })
		if err := stopUserWorker(dir, 20*time.Millisecond, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "99") {
			t.Fatalf("err = %v, want a timeout naming PID 99", err)
		}
	})
}

func TestLocalDoctorReportsUserWorker(t *testing.T) {
	base := func(uw userWorkerState) localDoctorDeps {
		return localDoctorDeps{
			discover:   func() (platform.BrowserInfo, error) { return platform.BrowserInfo{Kind: "chrome", Path: "/x"}, nil },
			bindTest:   func() error { return nil },
			vault:      func() (bool, error) { return false, nil },
			owner:      func() error { return nil },
			audit:      func() (localaudit.VerifyResult, error) { return localaudit.VerifyResult{}, nil },
			userWorker: func() userWorkerState { return uw },
			exe:        "citadel",
		}
	}
	find := func(checks []localDoctorCheck, what string) localDoctorCheck {
		for _, c := range checks {
			if c.What == what {
				return c
			}
		}
		t.Fatalf("no %q line in %+v", what, checks)
		return localDoctorCheck{}
	}
	this := `C:\Users\Test User\citadel.exe`
	value, _ := service.BuildRunValue(this)

	checks, failed := collectLocalDoctor(base(userWorkerState{}))
	if failed {
		t.Fatal("an absent worker failed the doctor")
	}
	if c := find(checks, "Autostart"); c.Status != "ok" || !strings.Contains(c.Detail, "off") {
		t.Fatalf("autostart off line = %+v", c)
	}
	if c := find(checks, "Worker"); c.Status != "ok" || !strings.Contains(c.Detail, "no background worker") {
		t.Fatalf("worker line = %+v", c)
	}

	checks, _ = collectLocalDoctor(base(userWorkerState{
		Autostart: service.UserAutostartStatus{Set: true, Value: value, Exe: this, PointsAtThis: true},
		Running:   true, SupervisorPID: 4242,
	}))
	if c := find(checks, "Autostart"); c.Status != "ok" || !strings.Contains(c.Detail, "on") {
		t.Fatalf("autostart on line = %+v", c)
	}
	if c := find(checks, "Worker"); !strings.Contains(c.Detail, "4242") || !strings.Contains(c.Detail, "worker stop") {
		t.Fatalf("running worker line = %+v", c)
	}

	checks, _ = collectLocalDoctor(base(userWorkerState{
		Autostart: service.UserAutostartStatus{Set: true, Value: `"C:\Old\citadel.exe" work --background`, Exe: `C:\Old\citadel.exe`},
	}))
	if c := find(checks, "Autostart"); c.Status != "note" || !strings.Contains(c.Detail, "different citadel") {
		t.Fatalf("stale autostart line = %+v", c)
	}

	checks, _ = collectLocalDoctor(base(userWorkerState{
		Autostart: service.UserAutostartStatus{Set: true, Value: value, Exe: this, PointsAtThis: true, DisabledByUser: true},
	}))
	if c := find(checks, "Autostart"); c.Status != "note" || !strings.Contains(c.Detail, "switched off") {
		t.Fatalf("disabled autostart line = %+v", c)
	}
}
