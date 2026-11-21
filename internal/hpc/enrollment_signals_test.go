package hpc

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestEnrollmentSignalPreservesCleanupAndTokenErrors(t *testing.T) {
	for _, kind := range []string{"term", "drain"} {
		for _, enrollErr := range []error{errors.New("issued credential cleanup failed; remove in console"), errors.New("token enrollment failed"), ErrApprovalInterrupted} {
			t.Run(kind+"/"+enrollErr.Error(), func(t *testing.T) {
				term, drain := make(chan os.Signal, 1), make(chan os.Signal, 1)
				target := term
				if kind == "drain" {
					target = drain
				}
				target <- os.Interrupt
				err := WithEnrollmentSignals(context.Background(), term, drain, func(ctx context.Context) error {
					select {
					case <-ctx.Done():
						return enrollErr
					case <-time.After(time.Second):
						t.Error("signal did not cancel enrollment")
						return errors.New("synthetic wait timed out")
					}
				})
				if err != enrollErr {
					t.Fatal("signal replaced the real enrollment or cleanup error")
				}
			})
		}
	}
}

func TestEnrollmentSignalRequeueNeverBlocksOnSecondSignal(t *testing.T) {
	for _, kind := range []string{"term", "drain"} {
		t.Run(kind, func(t *testing.T) {
			term, drain := make(chan os.Signal, 1), make(chan os.Signal, 1)
			target := term
			if kind == "drain" {
				target = drain
			}
			target <- os.Interrupt
			done := make(chan error, 1)
			go func() {
				done <- WithEnrollmentSignals(context.Background(), term, drain, func(ctx context.Context) error {
					<-ctx.Done()
					target <- os.Interrupt // fills the slot after the watcher consumed the first
					return nil             // approval and the signal raced, so shutdown still must run
				})
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				<-target // release a buggy blocking requeue before ending the test
				<-done
				t.Fatal("second signal blocked successful enrollment handoff")
			}
			select {
			case <-target:
			default:
				t.Fatal("shutdown signal was lost")
			}
		})
	}
}

func TestEnrollmentSignalRequeuesFirstSignal(t *testing.T) {
	term := make(chan os.Signal, 1)
	term <- os.Interrupt
	if err := WithEnrollmentSignals(context.Background(), term, nil, func(ctx context.Context) error { <-ctx.Done(); return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-term:
		if got != os.Interrupt {
			t.Fatal("wrong signal")
		}
	default:
		t.Fatal("first shutdown signal lost")
	}
}
