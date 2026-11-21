package hpc

import (
	"context"
	"os"
)

// WithEnrollmentSignals cancels enrollment when an allocation's termination or
// drain signal arrives. It preserves the callback's error, especially an issued
// credential's failed cleanup. A successful enrollment hands the signal back to
// the shutdown watcher. If another signal has filled the buffered channel, that
// pending signal already serves the same purpose; requeue must never block.
func WithEnrollmentSignals(ctx context.Context, term, drain chan os.Signal, enroll func(context.Context) error) error {
	enrollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped, done := make(chan struct{}), make(chan struct{})
	var gotTerm, gotDrain os.Signal
	go func() {
		defer close(done)
		select {
		case gotTerm = <-term:
			cancel()
		case gotDrain = <-drain:
			cancel()
		case <-stopped:
		}
	}()
	err := enroll(enrollCtx)
	close(stopped)
	<-done
	if err == nil {
		if gotTerm != nil {
			select {
			case term <- gotTerm:
			default:
			}
		}
		if gotDrain != nil {
			select {
			case drain <- gotDrain:
			default:
			}
		}
	}
	return err
}
