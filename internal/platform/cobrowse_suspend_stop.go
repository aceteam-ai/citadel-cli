package platform

import (
	"context"
	"errors"
)

// stopSuspendProc is only for a trusted, already-staged suspend transaction.
// The caller retains registry/action exclusion and fences the exact manager,
// session, generation and proc through every await. No production action invokes
// this helper yet. In particular, it never calls ordinary teardown, removes a
// profile or upgrades legacy best-effort stop into complete resource proof.
// The fence permits the expected root exit. Both injected operations must
// honor their supplied contexts; this helper does not abandon effect goroutines.
func stopSuspendProc(ctx context.Context, proc *cobrowseProc, fence func() error) error {
	return stopSuspendProcWith(ctx, proc, fence, closeSuspendBrowser)
}

func stopSuspendProcWith(ctx context.Context, proc *cobrowseProc, fence func() error,
	closeRoot func(context.Context, suspendBrowserClosePlan, func() error) error) error {
	fail := func() error { return errors.New("could not verify suspended browser resource teardown") }
	if ctx == nil || proc == nil || fence == nil || closeRoot == nil || proc.stopForSuspend == nil ||
		proc.exited == nil || proc.debugPort < 1 || proc.debugPort > 65535 || !validSuspendRestoredTargetID(proc.browserID) {
		return fail()
	}
	if _, finite := ctx.Deadline(); !finite || ctx.Err() != nil {
		return fail()
	}
	// One shared wall deadline covers graceful close and the positive adapter.
	// The graceful helper and owned-job adapter also cap their individual stages.
	ctx, cancel := context.WithTimeout(ctx, suspendGracefulCloseLimit+suspendJobProofLimit)
	defer cancel()
	plan := suspendBrowserClosePlan{port: proc.debugPort, browserID: proc.browserID, exited: proc.exited}
	proof := proc.stopForSuspend
	matches := func() bool {
		return proc.debugPort == plan.port && proc.browserID == plan.browserID && proc.exited == plan.exited
	}
	check := func() error {
		if ctx.Err() != nil || !matches() || fence() != nil || ctx.Err() != nil || !matches() {
			return fail()
		}
		return nil
	}
	if check() != nil {
		return fail()
	}
	// A disconnected/failed graceful request is not a teardown proof. It may
	// fall back only to this exact proc's positive adapter while still fenced.
	// Even a gracefully reaped root must pass complete resource verification.
	_ = closeRoot(ctx, plan, check)
	if check() != nil {
		return fail()
	}
	if proof(ctx) != nil || check() != nil {
		return fail()
	}
	select {
	case <-plan.exited:
	default:
		return fail()
	}
	return check()
}
