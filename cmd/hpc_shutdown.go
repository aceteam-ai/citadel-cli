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

// drain is the response to the early drain signal (SIGUSR1). A cancelled
// drainCtx (SIGTERM during the drain) ends the wait at once.
func (s *hpcShutdown) drain(drainCtx context.Context) {
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
			select {
			case <-drainCtx.Done():
				s.logf("hpc: drain interrupted by a termination signal")
				_ = s.deregisterOnce()
				if s.cancel != nil {
					s.cancel()
				}
				return
			case <-time.After(poll):
			}
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

// watchHPCSignals dispatches terminate and drain signals to the shutdown
// sequence and returns when it has finished or ctx ends. A terminate signal
// that arrives while a drain is waiting deregisters immediately: Slurm follows
// SIGTERM with SIGKILL after a short grace, and a drain can run for minutes.
func watchHPCSignals(ctx context.Context, term, drain <-chan os.Signal, s *hpcShutdown) {
	select {
	case <-term:
		fmt.Println("\n   - Received shutdown signal, deregistering...")
		s.terminate()
	case <-drain:
		fmt.Println("\n   - Received drain signal...")
		drainCtx, stopDrain := context.WithCancel(ctx)
		defer stopDrain()
		done := make(chan struct{})
		go func() {
			s.drain(drainCtx)
			close(done)
		}()
		select {
		case <-term:
			fmt.Println("\n   - Received shutdown signal during drain, deregistering...")
			stopDrain()
			s.terminate()
			<-done
		case <-done:
		case <-ctx.Done():
			stopDrain()
			<-done
		}
	case <-ctx.Done():
	}
}

// Indirections so the real-signal test registers exactly like production.
var (
	signalNotify = signal.Notify
	signalStop   = signal.Stop
)
