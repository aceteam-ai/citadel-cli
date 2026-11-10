package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"time"
)

// defaultHPCDrainTimeout bounds how long in-flight jobs may run after the
// drain signal. The launcher asks Slurm for the signal well before the wall
// end (aceteam#12718), so this only needs to fit inside that lead time.
const defaultHPCDrainTimeout = 120 * time.Second

// hpcDeregisterTimeout bounds the deregister call on every exit path.
const hpcDeregisterTimeout = 10 * time.Second

// hpcDrainTarget is the part of the runner the drain needs.
type hpcDrainTarget interface {
	Drain()
	ActiveJobs() int
}

// hpcShutdown sequences the HPC worker's exit: stop claiming, let in-flight
// work finish up to a bound, deregister (results still need the credential,
// so deregister comes last), then cancel. Deregistration runs at most once,
// whichever path reaches it first.
type hpcShutdown struct {
	deregister   func(context.Context) error
	cancel       func()
	drainTimeout time.Duration
	poll         time.Duration
	log          func(format string, args ...any)

	once     sync.Once
	deregErr error
	mu       sync.Mutex
	target   hpcDrainTarget
}

func (s *hpcShutdown) logf(format string, args ...any) {
	if s.log != nil {
		s.log(format, args...)
	}
}

// setTarget records the runner once it exists.
func (s *hpcShutdown) setTarget(t hpcDrainTarget) {
	s.mu.Lock()
	s.target = t
	s.mu.Unlock()
}

// deregisterOnce removes the node from the platform, once.
func (s *hpcShutdown) deregisterOnce() error {
	s.once.Do(func() {
		if s.deregister == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), hpcDeregisterTimeout)
		defer cancel()
		s.deregErr = s.deregister(ctx)
		if s.deregErr != nil {
			s.logf("hpc: deregister failed: %v", s.deregErr)
		} else {
			s.logf("hpc: node deregistered")
		}
	})
	return s.deregErr
}

// drain is the response to the early drain signal (SIGUSR1).
func (s *hpcShutdown) drain() {
	s.mu.Lock()
	target := s.target
	s.mu.Unlock()
	if target != nil {
		target.Drain()
		s.logf("hpc: draining, waiting up to %s for %d in-flight job(s)", s.drainTimeout, target.ActiveJobs())
		poll := s.poll
		if poll <= 0 {
			poll = 250 * time.Millisecond
		}
		deadline := time.Now().Add(s.drainTimeout)
		for target.ActiveJobs() > 0 && time.Now().Before(deadline) {
			time.Sleep(poll)
		}
		if n := target.ActiveJobs(); n > 0 {
			s.logf("hpc: drain timeout with %d job(s) still running", n)
		}
	}
	_ = s.deregisterOnce()
	if s.cancel != nil {
		s.cancel()
	}
}

// terminate is the response to SIGTERM/SIGINT: Slurm follows SIGTERM with
// SIGKILL after a short grace, so deregister immediately, then cancel.
func (s *hpcShutdown) terminate() {
	_ = s.deregisterOnce()
	if s.cancel != nil {
		s.cancel()
	}
}

// watchHPCSignals dispatches the first terminate or drain signal to the
// shutdown sequence and returns. It also returns when ctx ends.
func watchHPCSignals(ctx context.Context, term, drain <-chan os.Signal, s *hpcShutdown) {
	select {
	case <-term:
		fmt.Println("\n   - Received shutdown signal, deregistering...")
		s.terminate()
	case <-drain:
		fmt.Println("\n   - Received drain signal...")
		s.drain()
	case <-ctx.Done():
	}
}

// Indirections so the real-signal test registers exactly like production.
var (
	signalNotify = signal.Notify
	signalStop   = signal.Stop
)
