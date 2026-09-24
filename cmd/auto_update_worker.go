package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
)

// autoUpdatePolicyInputs contains only flags belonging to the active command.
// The control center has no --auto-update or interval flag; it passes zero values.
type autoUpdatePolicyInputs struct {
	force        bool
	intervalFlag string
}

func workAutoUpdatePolicyInputs() autoUpdatePolicyInputs {
	return autoUpdatePolicyInputs{force: workAutoUpdate, intervalFlag: workAutoUpdateInterval}
}

// resolveAutoUpdateEnabled is evaluated on every tick so persisted changes take
// effect without restarting the worker. Dev builds and explicit opt-out veto
// every enable signal, including the work-only flag.
func resolveAutoUpdateEnabled(inputs autoUpdatePolicyInputs) bool {
	if !autoUpdateAllowed() {
		return false
	}
	if inputs.force {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CITADEL_AUTO_UPDATE"))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	state, err := update.LoadState()
	return err == nil && state != nil && state.AutoUpdate
}

// resolveAutoUpdateInterval uses a work-only flag before the shared env value.
func resolveAutoUpdateInterval(inputs autoUpdatePolicyInputs) string {
	if inputs.intervalFlag != "" {
		return inputs.intervalFlag
	}
	return os.Getenv("CITADEL_AUTO_UPDATE_INTERVAL")
}

type autoUpdateWorker interface {
	ActiveJobs() int
	BeginDrain() (release func())
}

type autoUpdateRunner interface {
	autoUpdateWorker
	Run(context.Context) error
}

// autoUpdateRuntime is an injection seam for tests. Production uses the
// AutoUpdater's existing checksum, apply, restart, and Homebrew defaults.
type autoUpdateRuntime struct {
	checker     update.ReleaseChecker
	ticks       <-chan time.Time
	apply       func(string) error
	restart     func() error
	afterTick   func()
	afterCancel func()
}

// startWorkerAutoUpdater may be called only after a worker is fully initialized.
// ownsWorker is an explicit guard for attach-only/monitor-only control centers.
// A nil return means no loop was started (non-owner or invalid interval).
func startWorkerAutoUpdater(ctx context.Context, ownsWorker bool, inputs autoUpdatePolicyInputs, runner autoUpdateWorker, logf func(string, ...any), runtime autoUpdateRuntime) <-chan struct{} {
	if !ownsWorker || ctx.Err() != nil {
		return nil
	}
	interval, err := update.ParseInterval(resolveAutoUpdateInterval(inputs))
	if err != nil {
		logf("auto-update: %v; disabled", err)
		return nil
	}
	checker := runtime.checker
	if checker == nil {
		checker = update.NewClientWithTimeout(Version, 30*time.Second)
	}
	updater := update.NewAutoUpdater(update.AutoUpdaterConfig{
		Checker:    checker,
		Interval:   interval,
		Ticks:      runtime.ticks,
		AfterTick:  runtime.afterTick,
		Enabled:    func() bool { return resolveAutoUpdateEnabled(inputs) },
		ActiveJobs: runner.ActiveJobs,
		BeginDrain: runner.BeginDrain,
		Apply:      runtime.apply,
		Restart:    runtime.restart,
		Log:        logf,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		updater.Run(ctx)
	}()
	return done
}

// runWorkerWithAutoUpdater binds the updater to the actual runner lifetime,
// which may end without the parent context being cancelled (for example on a
// connect error). The updater is stopped and joined before ownership is freed
// or the caller may start a replacement worker. A nil owner runs without an
// updater, as with work --no-single-instance.
func runWorkerWithAutoUpdater(ctx context.Context, owner *worklock.Lock, inputs autoUpdatePolicyInputs, runner autoUpdateRunner, logf func(string, ...any), runtime autoUpdateRuntime) error {
	if owner != nil {
		defer owner.Release()
	}
	updaterCtx, cancelUpdater := context.WithCancel(ctx)
	done := startWorkerAutoUpdater(updaterCtx, owner != nil, inputs, runner, logf, runtime)
	defer func() {
		cancelUpdater()
		if runtime.afterCancel != nil {
			runtime.afterCancel()
		}
		if done != nil {
			<-done
		}
	}()
	return runner.Run(ctx)
}

func workAutoUpdateLog(format string, args ...any) {
	fmt.Printf("   - "+format+"\n", args...)
}
