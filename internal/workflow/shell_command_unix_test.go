//go:build linux || darwin

package workflow

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"
)

func TestWorkflowShellPrefersCompletedWaitOverCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	want := errors.New("ordinary command exit")
	waited := make(chan error, 1)
	waited <- want
	if got := awaitWorkflowShell(ctx, waited, func() error {
		t.Fatal("completed command group must not be signalled")
		return nil
	}); got != want {
		t.Fatal("completed Wait result changed")
	}
}

func TestWorkflowShellCancellationJoinsWholeWait(t *testing.T) {
	for _, killErr := range []error{nil, syscall.ESRCH, syscall.EPERM} {
		t.Run(fmt.Sprintf("signal=%v", killErr), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waited := make(chan error, 1)
			defer close(waited)
			signalled := make(chan struct{})
			finished := make(chan error, 1)
			go func() {
				finished <- awaitWorkflowShell(ctx, waited, func() error {
					close(signalled)
					return killErr
				})
			}()
			cancel()
			awaitWorkflowSignal(t, signalled)
			select {
			case <-finished:
				t.Fatal("cancellation returned before actual Wait completion")
			default:
			}
			want := errors.New("cancelled command exit")
			waited <- want
			select {
			case got := <-finished:
				if killErr == syscall.EPERM {
					if !errors.Is(got, killErr) {
						t.Fatal("signal failure was hidden after joining Wait")
					}
				} else if got != want {
					t.Fatal("Wait result changed on cancellation")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("watch did not return after Wait completed")
			}
		})
	}
}

func TestWorkflowShellAlreadyCancelledContextCannotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := newWorkflowShellCommand(ctx, "exit 0")
	if cmd.Cancel != nil {
		t.Fatal("built-in cancellation watcher was not disabled")
	}
	_, err := workflowShellCombinedOutput(ctx, cmd)
	if !errors.Is(err, context.Canceled) || cmd.Process != nil {
		t.Fatal("already-cancelled command was started")
	}
}
