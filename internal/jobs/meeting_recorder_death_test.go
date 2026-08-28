// internal/jobs/meeting_recorder_death_test.go
//
// Tests for citadel#490: a recorder (ffmpeg) that dies mid-call must be
// surfaced loudly (a returned error) rather than silently producing a
// truncated/empty WAV while the meeting loop keeps polling for meeting-end.
package jobs

import (
	"testing"
	"time"
)

// neverEndingPage is a meetingBrowser fake whose end heuristics never fire
// (meetIsEndedJS -> false, meetParticipantCountJS -> a count > 1), so the only
// way waitForMeetingEnd returns before the (very long) duration cap is via the
// recorder-death signal under test.
type neverEndingPage struct{}

func (neverEndingPage) Navigate(url string) error { return nil }
func (neverEndingPage) CurrentURL() (string, error) {
	return "https://meet.google.com/x", nil
}
func (neverEndingPage) Evaluate(expression string) (any, error) {
	switch expression {
	case meetIsEndedJS:
		return false, nil
	case meetParticipantCountJS:
		return float64(3), nil
	default:
		return nil, nil
	}
}
func (neverEndingPage) Type(selector, text string) error { return nil }
func (neverEndingPage) Close() error                     { return nil }

// deathTestMedia is a MeetingMedia stub whose RecordingAlive channel is
// supplied by the test, so it can be closed mid-loop to simulate the recorder
// process exiting unexpectedly.
type deathTestMedia struct {
	alive <-chan struct{}
}

func (deathTestMedia) Start() (meetingBrowser, error) { return neverEndingPage{}, nil }
func (deathTestMedia) StartRecording() error          { return nil }
func (deathTestMedia) StopRecording() (string, error) { return "/ws/meetings/m1.wav", nil }
func (deathTestMedia) Close() error                   { return nil }
func (m deathTestMedia) RecordingAlive() <-chan struct{} {
	return m.alive
}

// TestWaitForMeetingEnd_RecorderDeathSurfacesError pins the citadel#490 fix: if
// the recorder's death signal fires while the plain (non-streaming) meeting
// loop is still polling for meeting-end, waitForMeetingEnd must notice within
// one poll tick and return a non-nil error (not silently keep polling until the
// duration cap).
func TestWaitForMeetingEnd_RecorderDeathSurfacesError(t *testing.T) {
	died := make(chan struct{})
	media := deathTestMedia{alive: died}

	// Close the death channel shortly after the loop starts, simulating ffmpeg
	// exiting mid-call (not before the loop even begins polling).
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(died)
	}()

	start := time.Now()
	reason, err := (&MeetingJoinHandler{WorkspaceDir: "/ws"}).waitForMeetingEnd(
		JobContext{}, neverEndingPage{}, testParams(), media,
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForMeetingEnd returned a nil error after the recorder died mid-call; want a surfaced error")
	}
	if reason != "recorder_died" {
		t.Errorf("endReason = %q, want %q", reason, "recorder_died")
	}
	// The loop must catch the death signal promptly (well under the 5s poll
	// interval), not only at the next scheduled poll or the duration cap.
	if elapsed > 2*time.Second {
		t.Errorf("waitForMeetingEnd took %s to notice recorder death, want well under the poll interval", elapsed)
	}
}

// TestRunMeetingLoop_RecorderDeathPropagatesError verifies the error produced
// by a dead recorder actually propagates out of runMeetingLoop (the function
// MEETING_JOIN's Execute calls), for the plain (non-streaming) path.
func TestRunMeetingLoop_RecorderDeathPropagatesError(t *testing.T) {
	died := make(chan struct{})
	close(died) // already dead by the time the loop starts polling
	media := deathTestMedia{alive: died}

	h := &MeetingJoinHandler{WorkspaceDir: "/ws"} // StreamingEnabled left false
	outcome, err := h.runMeetingLoop(JobContext{}, neverEndingPage{}, testParams(), "/ws/meetings/m1.wav", media)

	if err == nil {
		t.Fatal("runMeetingLoop returned a nil error after the recorder died, want a surfaced error")
	}
	if outcome.endReason != "recorder_died" {
		t.Errorf("outcome.endReason = %q, want %q", outcome.endReason, "recorder_died")
	}
}

// TestWaitForMeetingEnd_NoRecorderDeathSignalDoesNotFalselyReport verifies a
// MeetingMedia backend that reports no liveness signal (RecordingAlive
// returns nil, e.g. the container backend — citadel#490's documented gap)
// never falsely reports a recorder death: the loop must still end normally
// via the existing DOM heuristic.
func TestWaitForMeetingEnd_NoRecorderDeathSignalDoesNotFalselyReport(t *testing.T) {
	media := deathTestMedia{alive: nil}
	page := &endingAfterFirstPollPage{}

	reason, err := (&MeetingJoinHandler{WorkspaceDir: "/ws"}).waitForMeetingEnd(
		JobContext{}, page, testParams(), media,
	)

	if err != nil {
		t.Fatalf("unexpected error with no recorder-death signal: %v", err)
	}
	if reason != "call_ended" {
		t.Errorf("endReason = %q, want %q", reason, "call_ended")
	}
}

// TestInteractive_RecorderDeathSurfacesError verifies the INTERACTIVE
// (StreamingEnabled) loop also detects and surfaces a recorder death, not just
// the plain waitForMeetingEnd path above. This matters most: production Meet
// meetings run this loop, not the plain one — config.Meeting.StreamingEnabled
// defaults true (internal/config/meeting.go).
func TestInteractive_RecorderDeathSurfacesError(t *testing.T) {
	h := newTestInteractiveHandler()
	page := &fakeMeetPage{} // empty chat, never ends on its own

	died := make(chan struct{})
	media := deathTestMedia{alive: died}
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(died)
	}()

	noSegments := func() ([]TranscriptSegment, error) { return nil, nil }
	out, err := h.waitForMeetingEndInteractive(
		JobContext{}, page, testParams(), noSegments, time.Millisecond, map[string]struct{}{}, media,
	)

	if err == nil {
		t.Fatal("waitForMeetingEndInteractive returned a nil error after the recorder died mid-call; want a surfaced error")
	}
	if out.endReason != "recorder_died" {
		t.Errorf("endReason = %q, want %q", out.endReason, "recorder_died")
	}
}

// endingAfterFirstPollPage reports the call as ended on the very first check,
// so the test above completes immediately rather than waiting a poll tick.
type endingAfterFirstPollPage struct{}

func (endingAfterFirstPollPage) Navigate(url string) error        { return nil }
func (endingAfterFirstPollPage) CurrentURL() (string, error)      { return "", nil }
func (endingAfterFirstPollPage) Type(selector, text string) error { return nil }
func (endingAfterFirstPollPage) Close() error                     { return nil }
func (endingAfterFirstPollPage) Evaluate(expression string) (any, error) {
	if expression == meetIsEndedJS {
		return true, nil
	}
	return nil, nil
}
