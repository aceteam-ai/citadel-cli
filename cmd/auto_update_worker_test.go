package cmd

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/update"
)

type updaterTestWorker struct {
	drains   atomic.Int32
	releases atomic.Int32
}

func (*updaterTestWorker) ActiveJobs() int { return 0 }
func (w *updaterTestWorker) BeginDrain() func() {
	w.drains.Add(1)
	return func() { w.releases.Add(1) }
}

type updaterTestChecker struct {
	checks    atomic.Int32
	downloads atomic.Int32
}

func (c *updaterTestChecker) CheckForUpdate() (*update.Release, error) {
	c.checks.Add(1)
	return &update.Release{TagName: "v9.9.9"}, nil
}
func (c *updaterTestChecker) DownloadAndVerify(*update.Release, string) error {
	c.downloads.Add(1)
	return nil
}

func TestWorkerAutoUpdaterOwnershipPolicyAndTicks(t *testing.T) {
	for _, tc := range []struct {
		name       string
		owns       bool
		inputs     autoUpdatePolicyInputs
		initial    bool
		wantChecks int32
	}{
		{name: "disabled work", owns: true},
		{name: "enabled work", owns: true, inputs: autoUpdatePolicyInputs{force: true}, wantChecks: 1},
		{name: "enabled control center owner", owns: true, initial: true, wantChecks: 1},
		{name: "attach-only control center", initial: true},
		{name: "work without ownership lock", inputs: autoUpdatePolicyInputs{force: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CITADEL_AUTO_UPDATE", "")
			t.Setenv("CITADEL_NO_AUTO_UPDATE", "")
			oldVersion, oldOptOut := Version, noAutoUpdate
			Version, noAutoUpdate = "v1.0.0", false
			t.Cleanup(func() { Version, noAutoUpdate = oldVersion, oldOptOut })
			if err := update.SaveState(&update.State{AutoUpdate: tc.initial}); err != nil {
				t.Fatal(err)
			}
			checker := &updaterTestChecker{}
			worker := &updaterTestWorker{}
			var applies atomic.Int32
			ticks, tickDone := make(chan time.Time), make(chan struct{}, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := startWorkerAutoUpdater(ctx, tc.owns, tc.inputs, worker,
				func(string, ...any) {}, autoUpdateRuntime{
					checker: checker, ticks: ticks, afterTick: func() { tickDone <- struct{}{} },
					apply:   func(string) error { applies.Add(1); return nil },
					restart: func() error { return nil },
				})
			if !tc.owns {
				if done != nil {
					t.Fatal("non-owner started an updater")
				}
			} else {
				if done == nil {
					t.Fatal("worker owner did not start an updater")
				}
				if checker.checks.Load() != 0 || checker.downloads.Load() != 0 || applies.Load() != 0 {
					t.Fatal("startup performed an update before the first tick")
				}
				tickUpdater(t, ticks, tickDone)
				cancel()
				waitUpdaterDone(t, done)
			}
			if got := checker.checks.Load(); got != tc.wantChecks {
				t.Errorf("checks = %d, want %d", got, tc.wantChecks)
			}
			if got := checker.downloads.Load(); got != tc.wantChecks {
				t.Errorf("downloads = %d, want %d", got, tc.wantChecks)
			}
			if got := applies.Load(); got != tc.wantChecks {
				t.Errorf("applies = %d, want %d", got, tc.wantChecks)
			}
			if got := worker.drains.Load(); got != tc.wantChecks {
				t.Errorf("drains = %d, want %d", got, tc.wantChecks)
			}
		})
	}
}

func TestWorkerAutoUpdaterPersistedToggleAndShutdown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CITADEL_AUTO_UPDATE", "")
	t.Setenv("CITADEL_NO_AUTO_UPDATE", "")
	oldVersion, oldOptOut := Version, noAutoUpdate
	Version, noAutoUpdate = "v1.0.0", false
	t.Cleanup(func() { Version, noAutoUpdate = oldVersion, oldOptOut })
	setEnabled := func(enabled bool) {
		t.Helper()
		if err := update.SaveState(&update.State{AutoUpdate: enabled}); err != nil {
			t.Fatal(err)
		}
	}
	setEnabled(false)
	checker := &updaterTestChecker{}
	var applies atomic.Int32
	ticks, tickDone := make(chan time.Time), make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := startWorkerAutoUpdater(ctx, true, autoUpdatePolicyInputs{}, &updaterTestWorker{},
		func(string, ...any) {}, autoUpdateRuntime{
			checker: checker, ticks: ticks, afterTick: func() { tickDone <- struct{}{} },
			apply:   func(string) error { applies.Add(1); return nil },
			restart: func() error { return nil },
		})
	if done == nil {
		t.Fatal("updater did not start")
	}
	tickUpdater(t, ticks, tickDone)
	if checker.checks.Load() != 0 || applies.Load() != 0 {
		t.Fatal("disabled tick attempted an update")
	}
	setEnabled(true)
	tickUpdater(t, ticks, tickDone)
	if checker.checks.Load() != 1 || applies.Load() != 1 {
		t.Fatal("enabled next tick did not attempt an update")
	}
	setEnabled(false)
	tickUpdater(t, ticks, tickDone)
	if checker.checks.Load() != 1 || applies.Load() != 1 {
		t.Fatal("disabled next tick still attempted an update")
	}
	cancel()
	waitUpdaterDone(t, done)
	if checker.checks.Load() != 1 {
		t.Fatal("shutdown allowed an additional check")
	}
}

func TestWorkerAutoUpdaterInvalidIntervalDoesNotStart(t *testing.T) {
	t.Setenv("CITADEL_AUTO_UPDATE_INTERVAL", "not-a-duration")
	checker := &updaterTestChecker{}
	done := startWorkerAutoUpdater(context.Background(), true, autoUpdatePolicyInputs{},
		&updaterTestWorker{}, func(string, ...any) {}, autoUpdateRuntime{checker: checker})
	if done != nil || checker.checks.Load() != 0 {
		t.Fatal("invalid interval started an updater")
	}
}

func TestResolveAutoUpdateIntervalUsesOnlyCommandFlags(t *testing.T) {
	t.Setenv("CITADEL_AUTO_UPDATE_INTERVAL", "45m")
	inputs := autoUpdatePolicyInputs{intervalFlag: "15m"}
	if got := resolveAutoUpdateInterval(inputs); got != "15m" {
		t.Errorf("work interval = %q, want 15m", got)
	}
	if got := resolveAutoUpdateInterval(autoUpdatePolicyInputs{}); got != "45m" {
		t.Errorf("control-center interval = %q, want 45m", got)
	}
	checker := &updaterTestChecker{}
	if done := startWorkerAutoUpdater(context.Background(), true,
		autoUpdatePolicyInputs{intervalFlag: "invalid"}, &updaterTestWorker{},
		func(string, ...any) {}, autoUpdateRuntime{checker: checker}); done != nil {
		t.Fatal("invalid work flag was ignored in favor of env interval")
	}
}

func tickUpdater(t *testing.T, ticks chan<- time.Time, done <-chan struct{}) {
	t.Helper()
	select {
	case ticks <- time.Now():
	case <-time.After(time.Second):
		t.Fatal("updater did not receive tick")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("updater did not complete tick")
	}
}

func waitUpdaterDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("updater did not stop on cancellation")
	}
}
