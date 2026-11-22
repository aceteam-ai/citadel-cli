package service

import (
	"context"
	"errors"
	"time"
)

// The per-user supervisor (citadel-cli#1867 C3) keeps `citadel work` running for
// a standard Windows user who cannot install the service. It is the parent of
// the real worker and restarts it the way a service manager would: after a
// non-zero exit (a crash, a self-heal exit, a remote worker restart) with capped
// exponential backoff, at once after the update-restart exit code, and never
// after a clean exit or a stop request. The loop is platform-neutral so it is
// tested with a fake child and a fake clock; the process plumbing is in cmd.

// SupervisedChild is one running worker process.
type SupervisedChild interface {
	// Done is closed when the process has exited.
	Done() <-chan struct{}
	// ExitCode is the exit code once Done is closed (-1 when unknown).
	ExitCode() int
	// Interrupt asks the worker to shut down gracefully.
	Interrupt() error
	// Kill ends the worker (and, on Windows, its job) without waiting.
	Kill() error
}

// SupervisorClock is the time source; tests substitute a fake.
type SupervisorClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Supervisor defaults.
const (
	DefaultSupervisorBaseBackoff = 2 * time.Second
	DefaultSupervisorMaxBackoff  = 5 * time.Minute
	// DefaultSupervisorStableAfter is how long a worker must stay up for its
	// next failure to start again from the base delay.
	DefaultSupervisorStableAfter = 10 * time.Minute
	// DefaultSupervisorStopGrace bounds a graceful stop before the worker is
	// killed. It is well above the worker's own 5s shutdown watchdog.
	DefaultSupervisorStopGrace = 20 * time.Second
	// DefaultSupervisorMinRestartUptime: a restart-code exit sooner than this
	// after start is treated as a failure, so a worker that keeps asking for a
	// restart cannot spin the supervisor.
	DefaultSupervisorMinRestartUptime = 30 * time.Second
)

// SupervisorConfig configures RunSupervisor. Start is required.
type SupervisorConfig struct {
	Start func() (SupervisedChild, error)
	Clock SupervisorClock
	Log   func(format string, args ...any)

	BaseBackoff      time.Duration
	MaxBackoff       time.Duration
	StableAfter      time.Duration
	StopGrace        time.Duration
	MinRestartUptime time.Duration
	// RestartExitCode is the code that asks for an immediate relaunch
	// (update.SupervisorRestartExitCode in production). Zero disables it.
	RestartExitCode int
}

// RunSupervisor starts the worker and keeps it running until it exits 0 or ctx
// is cancelled (a stop request). On cancellation the running worker is
// interrupted, then killed if it has not exited within StopGrace.
func RunSupervisor(ctx context.Context, cfg SupervisorConfig) error {
	if cfg.Start == nil {
		return errors.New("supervisor: no start function")
	}
	clock := cfg.Clock
	if clock == nil {
		clock = realClock{}
	}
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	base := durationOr(cfg.BaseBackoff, DefaultSupervisorBaseBackoff)
	maxDelay := durationOr(cfg.MaxBackoff, DefaultSupervisorMaxBackoff)
	stableAfter := durationOr(cfg.StableAfter, DefaultSupervisorStableAfter)
	grace := durationOr(cfg.StopGrace, DefaultSupervisorStopGrace)
	minRestart := durationOr(cfg.MinRestartUptime, DefaultSupervisorMinRestartUptime)

	backoff := base
	for {
		if ctx.Err() != nil {
			return nil
		}
		started := clock.Now()
		child, err := cfg.Start()
		code := -1
		if err != nil {
			logf("supervisor: could not start the worker: %v", err)
		} else {
			select {
			case <-child.Done():
				code = child.ExitCode()
			case <-ctx.Done():
				stopSupervisedChild(child, clock, grace, logf)
				return nil
			}
		}
		uptime := clock.Now().Sub(started)

		var delay time.Duration
		switch {
		case err == nil && code == 0:
			logf("supervisor: worker exited cleanly; not restarting")
			return nil
		case err == nil && cfg.RestartExitCode != 0 && code == cfg.RestartExitCode && uptime >= minRestart:
			logf("supervisor: worker asked for a restart; relaunching")
			backoff = base
		default:
			if err == nil && uptime >= stableAfter {
				backoff = base
			}
			delay = backoff
			backoff *= 2
			if backoff > maxDelay {
				backoff = maxDelay
			}
			if err == nil {
				logf("supervisor: worker exited with code %d after %s; restarting in %s", code, uptime.Round(time.Second), delay)
			} else {
				logf("supervisor: retrying in %s", delay)
			}
		}
		if delay > 0 {
			select {
			case <-clock.After(delay):
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// stopSupervisedChild interrupts the worker, waits up to grace, then kills it
// and waits a further grace for the process to be reaped.
func stopSupervisedChild(child SupervisedChild, clock SupervisorClock, grace time.Duration, logf func(string, ...any)) {
	logf("supervisor: stop requested; asking the worker to shut down")
	if err := child.Interrupt(); err != nil {
		logf("supervisor: interrupt failed: %v", err)
	}
	select {
	case <-child.Done():
		return
	case <-clock.After(grace):
	}
	logf("supervisor: worker did not stop within %s; killing it", grace)
	if err := child.Kill(); err != nil {
		logf("supervisor: kill failed: %v", err)
	}
	select {
	case <-child.Done():
	case <-clock.After(grace):
		logf("supervisor: worker still not reaped after kill")
	}
}

func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
