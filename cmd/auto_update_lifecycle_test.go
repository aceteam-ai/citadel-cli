package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
)

type lifecycleUpdateRunner struct {
	started chan struct{}
	returnC chan error
}

func (*lifecycleUpdateRunner) ActiveJobs() int { return 0 }
func (*lifecycleUpdateRunner) BeginDrain() func() {
	return func() {}
}
func (r *lifecycleUpdateRunner) Run(ctx context.Context) error {
	close(r.started)
	select {
	case err := <-r.returnC:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newLifecycleUpdateRunner() *lifecycleUpdateRunner {
	return &lifecycleUpdateRunner{started: make(chan struct{}), returnC: make(chan error, 1)}
}

func setLifecycleUpdatePolicy(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CITADEL_AUTO_UPDATE", "true")
	t.Setenv("CITADEL_NO_AUTO_UPDATE", "")
	oldVersion, oldOptOut := Version, noAutoUpdate
	Version, noAutoUpdate = "v1.0.0", false
	t.Cleanup(func() { Version, noAutoUpdate = oldVersion, oldOptOut })
}

func requireCompetingWorkerRefused(t *testing.T, stateDir string) {
	t.Helper()
	other, err := worklock.Acquire(stateDir, "v2.0.0", nil)
	if err == nil {
		other.Release()
		t.Fatal("later citadel work acquired lock while control-center updater owns it")
	}
	var alreadyRunning *worklock.ErrAlreadyRunning
	if !errors.As(err, &alreadyRunning) {
		t.Fatalf("competing worker error = %v, want ErrAlreadyRunning", err)
	}
}

func TestControlCenterOwnedRunnerBlocksLaterWorker(t *testing.T) {
	setLifecycleUpdatePolicy(t)
	stateDir := filepath.Join(t.TempDir(), "network")
	owner, held, _, err := acquireControlCenterWorkerLock(stateDir, nil)
	if err != nil || held || owner == nil {
		t.Fatalf("control-center acquire = (%v, held=%v, %v)", owner, held, err)
	}
	defer owner.Release()
	runner := newLifecycleUpdateRunner()
	checker := &updaterTestChecker{}
	ticks, tickDone := make(chan time.Time), make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- runWorkerWithAutoUpdater(ctx, owner, autoUpdatePolicyInputs{}, runner,
			func(string, ...any) {}, autoUpdateRuntime{checker: checker, ticks: ticks, afterTick: func() { tickDone <- struct{}{} },
				apply: func(string) error { return nil }, restart: func() error { return nil }})
	}()
	<-runner.started
	tickUpdater(t, ticks, tickDone)
	if checker.checks.Load() != 1 {
		t.Fatal("control-center owner did not run periodic updater")
	}
	requireCompetingWorkerRefused(t, stateDir)
	runner.returnC <- nil // Runner.Run stops without cancelling the parent context.
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatalf("runner returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("owned runner did not return")
	}
	if ctx.Err() != nil {
		t.Fatal("test parent context was cancelled; runner-return path was not tested")
	}
	next, err := worklock.Acquire(stateDir, "v2.0.0", nil)
	if err != nil {
		t.Fatalf("later worker could not acquire after runner/updater stopped: %v", err)
	}
	next.Release()
}

type blockingLifecycleChecker struct {
	started   chan struct{}
	release   chan struct{}
	downloads atomic.Int32
}

func (c *blockingLifecycleChecker) CheckForUpdate() (*update.Release, error) {
	close(c.started)
	<-c.release
	return &update.Release{TagName: "v9.9.9"}, nil
}
func (c *blockingLifecycleChecker) DownloadAndVerify(*update.Release, string) error {
	c.downloads.Add(1)
	return nil
}

func TestRunnerReturnWaitsForUpdaterBeforeReleasingOwner(t *testing.T) {
	setLifecycleUpdatePolicy(t)
	stateDir := filepath.Join(t.TempDir(), "network")
	owner, held, _, err := acquireControlCenterWorkerLock(stateDir, nil)
	if err != nil || held || owner == nil {
		t.Fatalf("control-center acquire = (%v, held=%v, %v)", owner, held, err)
	}
	defer owner.Release()
	runner := newLifecycleUpdateRunner()
	checker := &blockingLifecycleChecker{started: make(chan struct{}), release: make(chan struct{})}
	ticks := make(chan time.Time)
	var applied atomic.Bool
	updaterCancelled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- runWorkerWithAutoUpdater(ctx, owner, autoUpdatePolicyInputs{}, runner,
			func(string, ...any) {}, autoUpdateRuntime{checker: checker, ticks: ticks,
				apply: func(string) error { applied.Store(true); return nil }, restart: func() error { return nil },
				afterCancel: func() { close(updaterCancelled) }})
	}()
	<-runner.started
	select {
	case ticks <- time.Now():
	case <-time.After(time.Second):
		t.Fatal("updater did not receive tick")
	}
	select {
	case <-checker.started:
	case <-time.After(time.Second):
		t.Fatal("checker did not start")
	}
	runnerErr := errors.New("runner connection failed")
	runner.returnC <- runnerErr // Parent context stays live.
	select {
	case <-updaterCancelled:
	case <-time.After(time.Second):
		t.Fatal("Runner.Run return did not cancel updater")
	}
	select {
	case <-workerDone:
		t.Fatal("runner returned before in-flight updater stopped")
	default:
	}
	requireCompetingWorkerRefused(t, stateDir)
	close(checker.release)
	select {
	case err := <-workerDone:
		if !errors.Is(err, runnerErr) {
			t.Fatalf("runner error = %v, want original error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("updater did not stop after checker returned")
	}
	if ctx.Err() != nil || checker.downloads.Load() != 0 || applied.Load() {
		t.Fatal("runner return left an active updater or performed a cancelled install")
	}
	next, err := worklock.Acquire(stateDir, "v2.0.0", nil)
	if err != nil {
		t.Fatalf("ownership not released after updater stopped: %v", err)
	}
	next.Release()
}

func TestControlCenterAttachOnlyDoesNotAcquireOwner(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")
	dedicated, err := worklock.Acquire(stateDir, "v1.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dedicated.Release()
	owner, held, _, err := acquireControlCenterWorkerLock(stateDir, nil)
	if err != nil || !held || owner != nil {
		t.Fatalf("attached control center acquired owner: lock=%v held=%v err=%v", owner, held, err)
	}
}

func TestControlCenterLockErrorCannotStartWorker(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, held, _, err := acquireControlCenterWorkerLock(filepath.Join(parent, "network"), nil)
	if err == nil || held || owner != nil {
		t.Fatalf("lock error did not fail closed: owner=%v held=%v err=%v", owner, held, err)
	}
}
