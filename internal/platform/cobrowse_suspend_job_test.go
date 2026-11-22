package platform

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func suspendJobFixture() (*suspendJobStop, *[]string) {
	exited := make(chan struct{})
	close(exited)
	var order []string
	s := newSuspendJobStop(exited,
		func() error { order = append(order, "legacy"); return nil },
		func() error { order = append(order, "terminate"); return nil },
		func() (uint32, error) { order = append(order, "query"); return 0, nil },
		func() error { order = append(order, "close"); return nil })
	return s, &order
}

func TestSuspendJobRequiresWholeJobAndReaper(t *testing.T) {
	if suspendJobProofLimit != 5*time.Second {
		t.Fatal("job proof limit changed")
	}
	s, order := suspendJobFixture()
	counts := []uint32{2, 1, 0}
	s.active = func() (uint32, error) {
		*order = append(*order, "query")
		n := counts[0]
		counts = counts[1:]
		return n, nil
	}
	if err := s.stopForSuspend(suspendPagesContext(t)); err != nil ||
		!reflect.DeepEqual(*order, []string{"terminate", "query", "query", "query", "close"}) {
		t.Fatal("root exit was substituted for whole-job emptiness", err)
	}
	if len(s.gate) != 0 {
		t.Fatal("job proof leaked its exclusion token")
	}
	if err := s.stopForSuspend(suspendPagesContext(t)); err != nil {
		t.Fatal(err)
	}
	if err := s.ordinaryStop(); err != nil {
		t.Fatal(err)
	}
	if len(*order) != 5 || !s.closed || !s.emptyVerified {
		t.Fatal("verified teardown retried effects or lost its proof")
	}
}

func TestSuspendJobRefusalsAndRetry(t *testing.T) {
	for _, kind := range []string{"nil-context", "no-deadline", "cancelled", "missing-reaper", "missing-terminate", "missing-query", "missing-close", "terminate", "query", "root-alive", "children-alive", "close", "cancel-after-terminate", "cancel-after-query", "cancel-after-close"} {
		t.Run(kind, func(t *testing.T) {
			s, order := suspendJobFixture()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			privateErr := errors.New("private-job-sentinel")
			switch kind {
			case "nil-context":
				ctx = nil
			case "no-deadline":
				ctx = context.Background()
			case "cancelled":
				cancel()
			case "missing-reaper":
				s.exited = nil
			case "missing-terminate":
				s.terminate = nil
			case "missing-query":
				s.active = nil
			case "missing-close":
				s.close = nil
			case "terminate":
				s.terminate = func() error { return privateErr }
			case "query":
				s.active = func() (uint32, error) { return 0, privateErr }
			case "root-alive":
				s.exited = make(chan struct{})
			case "children-alive":
				s.active = func() (uint32, error) { return 1, nil }
			case "close":
				s.close = func() error { return privateErr }
			case "cancel-after-terminate":
				s.terminate = func() error { *order = append(*order, "terminate"); cancel(); return nil }
			case "cancel-after-query":
				s.active = func() (uint32, error) { *order = append(*order, "query"); cancel(); return 0, nil }
			case "cancel-after-close":
				s.close = func() error { cancel(); return nil }
			}
			if err := s.stopForSuspend(ctx); err == nil || strings.Contains(err.Error(), "private-job") {
				t.Fatal("unknown teardown was acknowledged or leaked provider detail")
			}
			if kind == "nil-context" || kind == "no-deadline" || kind == "cancelled" || strings.HasPrefix(kind, "missing-") {
				if len(*order) != 0 {
					t.Fatal("invalid admission touched the owned job")
				}
			}
			if kind != "cancel-after-close" && (s.closed || s.emptyVerified) {
				t.Fatal("failed proof became a permanent closed/success receipt")
			}
			if kind == "cancel-after-terminate" && !reflect.DeepEqual(*order, []string{"terminate"}) {
				t.Fatal("cancelled termination performed another provider call")
			}
			if kind == "cancel-after-query" && !reflect.DeepEqual(*order, []string{"terminate", "query"}) {
				t.Fatal("cancelled query closed the job")
			}
			// Repair only fake dependencies and reconcile under a fresh budget.
			exited := make(chan struct{})
			close(exited)
			s.exited = exited
			s.terminate = func() error { return nil }
			s.active = func() (uint32, error) { return 0, nil }
			s.close = func() error { return nil }
			if err := s.stopForSuspend(suspendPagesContext(t)); err != nil || !s.emptyVerified {
				t.Fatal("interrupted proof could not be retried", err)
			}
		})
	}
}

func TestSuspendJobLongBudgetHasCeiling(t *testing.T) {
	s, _ := suspendJobFixture()
	s.active = func() (uint32, error) { return 1, nil }
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.stopForSuspend(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("nonempty job acknowledged")
		}
	case <-time.After(8 * time.Second):
		t.Error("long caller budget bypassed job proof ceiling")
		cancel()
		<-done
	}
	if s.closed || s.emptyVerified {
		t.Fatal("ceiling forged an empty-job receipt")
	}
}

func TestSuspendJobLegacySuccessIsNotPositiveProof(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "error"}[failed], func(t *testing.T) {
			s, order := suspendJobFixture()
			legacyErr := error(nil)
			if failed {
				legacyErr = errors.New("legacy-result")
			}
			calls := 0
			s.legacy = func() error { calls++; return legacyErr }
			if err := s.ordinaryStop(); err != legacyErr {
				t.Fatal("legacy result changed")
			}
			if err := s.ordinaryStop(); err != legacyErr || calls != 1 {
				t.Fatal("legacy stop was not cached")
			}
			if err := s.stopForSuspend(suspendPagesContext(t)); err == nil || len(*order) != 0 || s.emptyVerified {
				t.Fatal("closed legacy job was promoted to positive proof")
			}
		})
	}
}

func TestSuspendJobWaitBudgetAndConcurrentStops(t *testing.T) {
	s, _ := suspendJobFixture()
	entered, release := make(chan struct{}), make(chan struct{})
	s.legacy = func() error { close(entered); <-release; return nil }
	done := make(chan error, 1)
	go func() { done <- s.ordinaryStop() }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.stopForSuspend(ctx); err == nil {
		t.Error("gate wait ignored original budget")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := s.stopForSuspend(suspendPagesContext(t)); err == nil {
		t.Fatal("legacy race forged proof")
	}
	// Conversely, a positive proof is shared without a second termination,
	// query or close when ordinary and suspend callers arrive together.
	s, _ = suspendJobFixture()
	var effects atomic.Int32
	s.terminate = func() error { effects.Add(1); return nil }
	s.active = func() (uint32, error) { effects.Add(1); return 0, nil }
	s.close = func() error { effects.Add(1); return nil }
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errorsSeen <- s.stopForSuspend(suspendPagesContext(t)) }()
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ordinaryStop(); err != nil || effects.Load() != 3 {
		t.Fatal("concurrent teardown repeated effects", err)
	}
}

func TestSuspendJobFailureStillAllowsLegacyCleanup(t *testing.T) {
	s, order := suspendJobFixture()
	s.active = func() (uint32, error) { return 0, errors.New("unknown") }
	if err := s.stopForSuspend(suspendPagesContext(t)); err == nil {
		t.Fatal("query failure acknowledged")
	}
	if err := s.ordinaryStop(); err != nil || !reflect.DeepEqual(*order, []string{"terminate", "legacy"}) {
		t.Fatal("failed suspend poisoned existing cleanup", err)
	}
}
