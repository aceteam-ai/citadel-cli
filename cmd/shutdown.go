// cmd/shutdown.go
package cmd

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// shutdownStep is a single named teardown action run during a graceful shutdown.
// Each step is run in its own goroutine so a misbehaving (blocking) step cannot
// stall the others or the overall deadline.
type shutdownStep struct {
	name string
	fn   func()
}

// runShutdownSequence runs all steps concurrently and waits up to grace for them
// to complete. It returns the names of any steps that had NOT finished when the
// grace period elapsed (empty slice means a clean shutdown).
//
// This is the bounded safety net for issue #312: shutdown must never block
// indefinitely. The caller decides what to do with a non-empty result (the TUI
// path force-exits via os.Exit). Keeping the timeout logic here — pure and
// side-effect free apart from running the steps — makes it unit-testable with a
// deliberately-stuck stub step.
func runShutdownSequence(grace time.Duration, steps []shutdownStep) []string {
	if len(steps) == 0 {
		return nil
	}

	var mu sync.Mutex
	pending := make(map[string]struct{}, len(steps))
	for _, s := range steps {
		pending[s.name] = struct{}{}
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(len(steps))
	for _, s := range steps {
		go func(step shutdownStep) {
			defer wg.Done()
			if step.fn != nil {
				step.fn()
			}
			mu.Lock()
			delete(pending, step.name)
			mu.Unlock()
		}(s)
	}
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(grace):
		mu.Lock()
		stuck := make([]string, 0, len(pending))
		for name := range pending {
			stuck = append(stuck, name)
		}
		mu.Unlock()
		return stuck
	}
}

// gracefulShutdown runs the teardown steps with a bounded grace period and, if
// any step is still pending when the period elapses, logs the stragglers and
// force-exits the process so a wedged goroutine can never leave an orphaned
// node holding the VPN identity / terminal port (issue #312).
//
// The grace period is intentionally short (seconds) because every individual
// subsystem teardown is itself bounded; the watchdog only fires if something
// regresses.
const shutdownGracePeriod = 5 * time.Second

func gracefulShutdown(steps []shutdownStep) {
	stuck := runShutdownSequence(shutdownGracePeriod, steps)
	if len(stuck) == 0 {
		return
	}
	for _, name := range stuck {
		Log("shutdown: step %q did not complete within %s; force-exiting", name, shutdownGracePeriod)
		fmt.Fprintf(os.Stderr, "   - Warning: shutdown step %q timed out; force-exiting\n", name)
	}
	os.Exit(1)
}
