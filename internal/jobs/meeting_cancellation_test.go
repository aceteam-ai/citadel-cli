// internal/jobs/meeting_cancellation_test.go
//
// Tests for the cancellation half of citadel#488: a hung/in-flight meeting
// bot must be interruptible via the job's context, not just wall-clock
// timeouts. Before this fix, none of the meeting join/wait loops
// (pollForJoinClick, waitUntilAdmitted, waitForMeetingEnd,
// waitForMeetingEndInteractive, and their Teams counterparts
// pollForTeamsJoinClick/waitUntilTeamsAdmitted) observed context cancellation
// at all, so a worker shutdown or drain could not interrupt an in-flight
// meeting short of its (up to 4h) duration cap or a SIGKILL that skips every
// defer.
//
// The orphan-reaper half of #488 (reclaiming leaked Chrome/Xvfb/profile-dir/
// audio-sink resources left behind by a hard kill) is explicitly out of scope
// here and tracked separately.
package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

// promptReturnBudget bounds how long a cancelled wait loop is allowed to take
// to notice and return. All three loops poll every meetingPollInterval
// (5s) / admitTimeout-scaled cadence in production, but a cancelled context
// must short-circuit the select immediately -- well under one real poll tick
// -- rather than only being noticed at the next scheduled wakeup.
const promptReturnBudget = 2 * time.Second

// TestWaitUntilAdmitted_ContextCancelledReturnsPromptly pins that a cancelled
// job context interrupts the lobby-admission wait immediately instead of
// blocking up to admitTimeout (3 minutes).
func TestWaitUntilAdmitted_ContextCancelledReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the loop starts

	h := &MeetingJoinHandler{WorkspaceDir: "/ws"}
	jobCtx := JobContext{Ctx: ctx}

	start := time.Now()
	err := h.waitUntilAdmitted(jobCtx, neverEndingPage{}, testParams())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitUntilAdmitted returned a nil error on a cancelled context; want a surfaced cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if elapsed > promptReturnBudget {
		t.Errorf("waitUntilAdmitted took %s to notice cancellation, want under %s", elapsed, promptReturnBudget)
	}
}

// TestWaitUntilAdmitted_ContextCancelledMidLoopReturnsPromptly is the same
// contract, but cancellation happens shortly AFTER the loop has already begun
// polling (not before it starts), matching a real shutdown mid-lobby-wait.
func TestWaitUntilAdmitted_ContextCancelledMidLoopReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	h := &MeetingJoinHandler{WorkspaceDir: "/ws"}
	jobCtx := JobContext{Ctx: ctx}

	start := time.Now()
	err := h.waitUntilAdmitted(jobCtx, neverEndingPage{}, testParams())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitUntilAdmitted returned a nil error after mid-loop cancellation; want a surfaced cancellation error")
	}
	if elapsed > promptReturnBudget {
		t.Errorf("waitUntilAdmitted took %s to notice mid-loop cancellation, want under %s", elapsed, promptReturnBudget)
	}
}

// TestWaitForMeetingEnd_ContextCancelledReturnsPromptly pins the plain
// (non-streaming) record-until-end loop: a cancelled context must be noticed
// within one poll tick, same contract as the existing recorder-death signal
// (citadel#490), and must NOT be misreported as a recorder death.
func TestWaitForMeetingEnd_ContextCancelledReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	// No recorder-death signal in play: nil alive channel never fires, so any
	// early return must come from the ctx.Done() case, not recorderDead.
	media := deathTestMedia{alive: nil}
	jobCtx := JobContext{Ctx: ctx}

	start := time.Now()
	reason, err := (&MeetingJoinHandler{WorkspaceDir: "/ws"}).waitForMeetingEnd(
		jobCtx, neverEndingPage{}, testParams(), media,
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForMeetingEnd returned a nil error on a cancelled context; want a surfaced cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if reason != "cancelled" {
		t.Errorf("endReason = %q, want %q", reason, "cancelled")
	}
	if elapsed > promptReturnBudget {
		t.Errorf("waitForMeetingEnd took %s to notice cancellation, want under %s", elapsed, promptReturnBudget)
	}
}

// TestWaitForMeetingEndInteractive_ContextCancelledReturnsPromptly is the same
// contract for the INTERACTIVE (StreamingEnabled) loop -- the loop a
// production Meet meeting actually runs by default.
func TestWaitForMeetingEndInteractive_ContextCancelledReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	h := newTestInteractiveHandler()
	page := &fakeMeetPage{} // empty chat, never ends on its own
	media := deathTestMedia{alive: nil}
	jobCtx := JobContext{Ctx: ctx}
	noSegments := func() ([]TranscriptSegment, error) { return nil, nil }

	start := time.Now()
	out, err := h.waitForMeetingEndInteractive(
		jobCtx, page, testParams(), noSegments, time.Millisecond, map[string]struct{}{}, media,
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForMeetingEndInteractive returned a nil error on a cancelled context; want a surfaced cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if out.endReason != "cancelled" {
		t.Errorf("endReason = %q, want %q", out.endReason, "cancelled")
	}
	if elapsed > promptReturnBudget {
		t.Errorf("waitForMeetingEndInteractive took %s to notice cancellation, want under %s", elapsed, promptReturnBudget)
	}
}

// TestWaitForMeetingEnd_NoCancellationStillWorks is a regression guard: a
// JobContext with no Ctx set (the zero value, as legacy/synchronous callers
// and most other tests in this package construct it) must behave exactly as
// before -- JobContext.Context() falls back to context.Background(), whose
// Done() channel is nil and never fires, so the new select case can never
// spuriously fire for a caller that never wired a context in.
func TestWaitForMeetingEnd_NoCancellationStillWorks(t *testing.T) {
	media := deathTestMedia{alive: nil}
	page := &endingAfterFirstPollPage{}

	reason, err := (&MeetingJoinHandler{WorkspaceDir: "/ws"}).waitForMeetingEnd(
		JobContext{}, page, testParams(), media,
	)

	if err != nil {
		t.Fatalf("unexpected error with a zero-value JobContext: %v", err)
	}
	if reason != "call_ended" {
		t.Errorf("endReason = %q, want %q", reason, "call_ended")
	}
}

// TestRunMeetingLoop_ContextCancelledPropagatesThroughExecute verifies the
// cancellation error produced by waitForMeetingEnd propagates out of
// runMeetingLoop (the function MEETING_JOIN's Execute calls, whose deferred
// media.Close()/StopRecording() are what actually reclaim the browser/Xvfb/
// sink -- the failure mode #488 exists to fix), for the plain
// (non-streaming) path.
func TestRunMeetingLoop_ContextCancelledPropagatesThroughExecute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	media := deathTestMedia{alive: nil}
	h := &MeetingJoinHandler{WorkspaceDir: "/ws"} // StreamingEnabled left false
	jobCtx := JobContext{Ctx: ctx}

	start := time.Now()
	outcome, err := h.runMeetingLoop(jobCtx, neverEndingPage{}, testParams(), "/ws/meetings/m1.wav", media)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("runMeetingLoop returned a nil error after context cancellation, want a surfaced error")
	}
	if outcome.endReason != "cancelled" {
		t.Errorf("outcome.endReason = %q, want %q", outcome.endReason, "cancelled")
	}
	if elapsed > promptReturnBudget {
		t.Errorf("runMeetingLoop took %s to notice cancellation, want under %s", elapsed, promptReturnBudget)
	}
}

// TestPollForJoinClick_ContextCancelledReturnsPromptly pins the pre-join
// poll loop (runs BEFORE waitUntilAdmitted in runMeetJoinFlow): a cancelled
// context must be noticed within one poll tick rather than blocking up to
// joinButtonTimeout. neverEndingPage's Evaluate returns (nil, nil) for every
// expression this loop probes, so admission never trips and the join button
// never "clicks" -- the only way this call returns is the new cancellation
// case (or the real timeout, which promptReturnBudget rules out).
func TestPollForJoinClick_ContextCancelledReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := pollForJoinClick(JobContext{Ctx: ctx}, neverEndingPage{}, "Bot", time.Hour, time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("pollForJoinClick returned a nil error after context cancellation, want a surfaced error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if elapsed > promptReturnBudget {
		t.Errorf("pollForJoinClick took %s to notice cancellation, want under %s", elapsed, promptReturnBudget)
	}
}

// TestPollForTeamsJoinClick_ContextCancelledReturnsPromptly is the Teams
// counterpart of TestPollForJoinClick_ContextCancelledReturnsPromptly.
func TestPollForTeamsJoinClick_ContextCancelledReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := pollForTeamsJoinClick(JobContext{Ctx: ctx}, neverEndingPage{}, "Bot", "", time.Hour, time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("pollForTeamsJoinClick returned a nil error after context cancellation, want a surfaced error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if elapsed > promptReturnBudget {
		t.Errorf("pollForTeamsJoinClick took %s to notice cancellation, want under %s", elapsed, promptReturnBudget)
	}
}

// TestWaitUntilTeamsAdmitted_ContextCancelledReturnsPromptly is the Teams
// counterpart of TestWaitUntilAdmitted_ContextCancelledReturnsPromptly.
func TestWaitUntilTeamsAdmitted_ContextCancelledReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	h := &MeetingJoinHandler{WorkspaceDir: "/ws"}
	jobCtx := JobContext{Ctx: ctx}

	start := time.Now()
	err := h.waitUntilTeamsAdmitted(jobCtx, neverEndingPage{}, testParams())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitUntilTeamsAdmitted returned a nil error after context cancellation, want a surfaced error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if elapsed > promptReturnBudget {
		t.Errorf("waitUntilTeamsAdmitted took %s to notice cancellation, want under %s", elapsed, promptReturnBudget)
	}
}
