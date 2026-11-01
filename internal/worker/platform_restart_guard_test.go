package worker

import (
	"context"
	"testing"
)

func TestPlatformRestartGuardRejectsAnyAdmittedWorkOrOtherDrain(t *testing.T) {
	isolateSelfHealHome(t)
	for _, tc := range []struct {
		name string
		r    *Runner
	}{
		{name: "running", r: &Runner{activeJobs: 1}},
		{name: "claim handoff", r: &Runner{dispatchingJobs: 1}},
		{name: "polled pending job", r: &Runner{pendingJob: &Job{}}},
		{name: "permanent drain", r: &Runner{permanentDrain: true}},
		{name: "other temporary owner", r: &Runner{drainScopes: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			if tc.r.TryIdleRestart(func() { called = true }) || called {
				t.Fatal("restart interrupted admitted work or another drain")
			}
		})
	}
	r := &Runner{}
	called := false
	if !r.TryIdleRestart(func() {
		called = true
		if r.drainScopes != 1 || r.drainMu.TryLock() {
			t.Fatal("exit action ran without its admission fence")
		}
	}) || !called || r.drainScopes != 0 {
		t.Fatal("idle restart did not execute/release exactly its own fence")
	}
}

func TestPlatformRestartGuardWaitsForPollClaimHandoff(t *testing.T) {
	isolateSelfHealHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r := &Runner{pollCancel: cancel, pollDone: done}
	go func() {
		<-ctx.Done()
		r.drainMu.Lock()
		r.pendingJob = &Job{} // A source delivery won the race with poll cancellation.
		r.pollCancel, r.pollDone = nil, nil
		r.drainMu.Unlock()
		close(done)
	}()
	if r.TryIdleRestart(func() { t.Error("exit raced a poll's admitted claim") }) {
		t.Fatal("pending claim was not fenced")
	}
	if r.drainScopes != 0 {
		t.Fatal("restart veto leaked its temporary drain")
	}
}
