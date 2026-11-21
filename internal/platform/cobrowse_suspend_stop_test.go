package platform

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSuspendStopRequiresCompleteProofAfterGracefulClose(t *testing.T) {
	for _, graceful := range []bool{false, true} {
		t.Run(map[bool]string{true: "graceful", false: "fallback"}[graceful], func(t *testing.T) {
			f := newSuspendCloseFixture()
			proc := &cobrowseProc{browserID: f.plan.browserID, debugPort: f.plan.port, exited: f.exited}
			legacy, proofs := 0, 0
			proc.stop = func() error { legacy++; return nil }
			proc.stopForSuspend = func(ctx context.Context) error {
				proofs++
				f.step("positive-proof")
				if ctx.Err() != nil {
					t.Fatal("positive proof lost its budget")
				}
				if !graceful {
					select {
					case <-f.exited:
					default:
						close(f.exited)
					}
				}
				return nil
			}
			f.after = func(phase string) {
				if phase == "call" {
					close(f.exited)
				}
			}
			ctx := suspendPagesContext(t)
			source := f.source(t, ctx)
			if !graceful {
				source.endpoint = func(context.Context, int) (string, error) {
					f.step("endpoint")
					return "", errors.New("private-stop-sentinel")
				}
			}
			closeRoot := func(ctx context.Context, plan suspendBrowserClosePlan, fence func() error) error {
				return closeSuspendBrowserWith(ctx, plan, fence, source)
			}
			if err := stopSuspendProcWith(ctx, proc, f.fence, closeRoot); err != nil || proofs != 1 || legacy != 0 {
				t.Fatal("root-close result substituted for complete resource proof", err)
			}
			want := []string{"endpoint", "open", "call", "close", "positive-proof"}
			if !graceful {
				want = []string{"endpoint", "positive-proof"}
			}
			if !reflect.DeepEqual(f.calls, want) {
				t.Fatal("positive proof preceded graceful close or leaked its connection", f.calls)
			}
			f.calls = nil
			if err := stopSuspendProcWith(ctx, proc, f.fence, closeRoot); err != nil || proofs != 2 ||
				!reflect.DeepEqual(f.calls, []string{"positive-proof"}) {
				t.Fatal("already-reaped retry skipped resource proof or touched CDP", err)
			}
		})
	}
}

func TestSuspendStopRefusesBeforeAnyEffect(t *testing.T) {
	for _, kind := range []string{"nil-context", "unbounded", "cancelled", "nil-proc", "nil-fence", "stale", "nil-close", "legacy-only", "no-reaper", "bad-port", "bad-browser"} {
		t.Run(kind, func(t *testing.T) {
			f := newSuspendCloseFixture()
			proc := &cobrowseProc{browserID: f.plan.browserID, debugPort: f.plan.port, exited: f.exited}
			calls := 0
			proc.stop = func() error { calls++; return nil }
			proc.stopForSuspend = func(context.Context) error { calls++; return nil }
			ctx, fence := suspendPagesContext(t), f.fence
			closeRoot := func(context.Context, suspendBrowserClosePlan, func() error) error { calls++; return nil }
			switch kind {
			case "nil-context":
				ctx = nil
			case "unbounded":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "nil-proc":
				proc = nil
			case "nil-fence":
				fence = nil
			case "stale":
				f.stale = true
			case "nil-close":
				closeRoot = nil
			case "legacy-only":
				proc.stopForSuspend = nil
			case "no-reaper":
				proc.exited = nil
			case "bad-port":
				proc.debugPort = 65536
			case "bad-browser":
				proc.browserID = "other/browser"
			}
			if err := stopSuspendProcWith(ctx, proc, fence, closeRoot); err == nil || calls != 0 || strings.Contains(err.Error(), "private-") {
				t.Fatal("invalid resource-stop plan reached an effect or leaked detail", err)
			}
		})
	}
}

func TestSuspendStopFencesCloseAndProof(t *testing.T) {
	for _, kind := range []string{"close-stale", "close-cancel", "close-port", "close-browser", "close-reaper", "proof-stale", "proof-cancel", "proof-error", "proof-no-reap", "fence-cancels", "final-fence-port"} {
		t.Run(kind, func(t *testing.T) {
			f := newSuspendCloseFixture()
			ctx, cancel := context.WithCancel(suspendPagesContext(t))
			defer cancel()
			proc := &cobrowseProc{browserID: f.plan.browserID, debugPort: f.plan.port, exited: f.exited}
			proofs, legacy := 0, 0
			proc.stop = func() error { legacy++; return nil }
			closeRoot := func(context.Context, suspendBrowserClosePlan, func() error) error {
				switch kind {
				case "close-stale":
					f.stale = true
				case "close-cancel":
					cancel()
				case "close-port":
					proc.debugPort++
				case "close-browser":
					proc.browserID += "-changed"
				case "close-reaper":
					proc.exited = make(chan struct{})
				}
				return nil
			}
			proc.stopForSuspend = func(context.Context) error {
				proofs++
				if kind == "proof-stale" {
					f.stale = true
				}
				if kind == "proof-cancel" {
					cancel()
				}
				if kind != "proof-no-reap" {
					close(f.exited)
				}
				if kind == "proof-error" {
					return errors.New("private-stop-sentinel")
				}
				return nil
			}
			fence := f.fence
			if kind == "fence-cancels" {
				fence = func() error { cancel(); return nil }
			}
			if kind == "final-fence-port" {
				checks := 0
				fence = func() error {
					checks++
					if checks == 4 {
						proc.debugPort++
					}
					return nil
				}
			}
			if err := stopSuspendProcWith(ctx, proc, fence, closeRoot); err == nil || legacy != 0 || strings.Contains(err.Error(), "private-") {
				t.Fatal("stale/uncertain stop acknowledged or used ordinary teardown", err)
			}
			want := 0
			if strings.HasPrefix(kind, "proof-") || kind == "final-fence-port" {
				want = 1
			}
			if proofs != want {
				t.Fatal("proof ran after a refused close boundary")
			}
		})
	}
}

func TestSuspendStopSharesOriginalBoundedBudget(t *testing.T) {
	for _, budget := range []time.Duration{time.Hour, time.Second} {
		t.Run(budget.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			parentDeadline, _ := ctx.Deadline()
			f := newSuspendCloseFixture()
			proc := &cobrowseProc{browserID: f.plan.browserID, debugPort: f.plan.port, exited: f.exited}
			var gracefulContext context.Context
			closeRoot := func(ctx context.Context, _ suspendBrowserClosePlan, _ func() error) error {
				deadline, finite := ctx.Deadline()
				if !finite || deadline.After(parentDeadline) || time.Until(deadline) > suspendGracefulCloseLimit+suspendJobProofLimit {
					t.Error("unbounded composite stop")
				}
				gracefulContext = ctx
				return errors.New("graceful result is not proof")
			}
			proc.stopForSuspend = func(ctx context.Context) error {
				if ctx != gracefulContext {
					t.Error("positive stop received a fresh budget")
				}
				close(f.exited)
				return nil
			}
			if err := stopSuspendProcWith(ctx, proc, f.fence, closeRoot); err != nil {
				t.Fatal(err)
			}
		})
	}
	// The default graceful adapter must also run the positive proof on a
	// reaped retry, without dialing any debugger endpoint.
	f := newSuspendCloseFixture()
	close(f.exited)
	proofs := 0
	proc := &cobrowseProc{browserID: f.plan.browserID, debugPort: f.plan.port, exited: f.exited,
		stopForSuspend: func(context.Context) error { proofs++; return nil }}
	if err := stopSuspendProc(suspendPagesContext(t), proc, f.fence); err != nil || proofs != 1 {
		t.Fatal(err)
	}
}

func TestSuspendStopComposesWithDurableCaptureCompletion(t *testing.T) {
	for _, proofError := range []bool{false, true} {
		t.Run(map[bool]string{true: "uncertain-proof", false: "complete"}[proofError], func(t *testing.T) {
			s, store, live, source, _ := stagedCaptureFixture(t)
			exited := make(chan struct{})
			s.proc.exited = exited
			proofs := 0
			s.proc.stopForSuspend = func(context.Context) error {
				proofs++
				close(exited)
				if proofError {
					return errors.New("synthetic uncertain resource proof")
				}
				return nil
			}
			ctx := suspendPagesContext(t)
			err := s.withStagedThrowawayCapture(ctx, store, live, suspendTestNow, false, source,
				func() error { return nil }, func(intent sessionRecord) error {
					_, err := store.completeSuspending(ctx, intent, suspendTestNow.Add(time.Second),
						func(ctx context.Context, identity suspendStopIdentity) error {
							if identity != suspendIdentity(live) {
								t.Fatal("completion changed the stopped incarnation")
							}
							return stopSuspendProcWith(ctx, s.proc, func() error { return nil },
								func(context.Context, suspendBrowserClosePlan, func() error) error {
									return errors.New("synthetic graceful disconnect")
								})
						})
					return err
				})
			final, readErr := store.read(live.ID)
			want := RecordSuspended
			if proofError {
				want = "suspending"
			}
			if (err != nil) != proofError || readErr != nil || proofs != 1 || final.State != want || final.Generation != live.Generation ||
				store.payloads.checkPayload(final) != nil {
				t.Fatal("resource proof did not precede durable dormant publication", err, readErr)
			}
			assertStagedCaptureUnlocked(t, s, store)
		})
	}
}
