package jobs

import (
	"context"
	"errors"
	"fmt"
)

// Meeting progress stages (#1506, S4a), published on the job's existing
// progress stream. A stage change always passes the throttle.
const (
	MeetingStageJoining          = "joining"
	MeetingStageWaitingAdmission = "waiting_admission"
	MeetingStageAdmitted         = "admitted"
	MeetingStageTranscribing     = "transcribing"
	MeetingStageEnded            = "ended"
)

// End reasons form a closed set: a progress event never carries page text,
// URLs or names. Unknown internal reasons publish as "ended".
const (
	MeetingEndDenied        = "denied"
	MeetingEndAdmitTimeout  = "admit_timeout"
	MeetingEndStoppedByUser = "stopped_by_user"
)

var meetingEndReasons = map[string]bool{
	"call_ended": true, "alone_in_call": true, "nobody_joined": true,
	"max_duration_reached": true, "ace_command_leave": true, "recorder_died": true,
	"cancelled": true, MeetingEndDenied: true, MeetingEndAdmitTimeout: true,
	MeetingEndStoppedByUser: true, "join_failed": true, "recording_failed": true,
}

var (
	// ErrMeetingDenied is an explicit lobby denial, distinct from a timeout.
	ErrMeetingDenied = errors.New("meeting host denied the bot's request to join")
	// errMeetingAdmitTimeout marks an admission wait that expired undecided.
	errMeetingAdmitTimeout = errors.New("not admitted before the admission timeout")
	// errMeetingStopped is a Stop request before recording began. It wraps
	// context.Canceled so the runner reports it as a caller cancellation.
	errMeetingStopped = fmt.Errorf("meeting stopped by user before recording: %w", context.Canceled)
)

// Capabilities declares the graceful Stop (cooperative cancel) and the progress
// vocabulary. Lane and Deadline stay unset so the meeting-specific budget and
// the long-session lane maps still decide them.
func (h *MeetingJoinHandler) Capabilities() JobCapabilities {
	return JobCapabilities{
		CooperativeCancel: true,
		Stages: []string{MeetingStageJoining, MeetingStageWaitingAdmission, MeetingStageAdmitted,
			MeetingStageTranscribing, MeetingStageEnded},
	}
}

func meetingStage(ctx JobContext, stage string) {
	ctx.EmitProgress(ProgressEvent{Stage: ProgressStage(stage), Flush: true})
}

func meetingEnded(ctx JobContext, reason string) {
	if !meetingEndReasons[reason] {
		reason = ""
	}
	ctx.EmitProgress(ProgressEvent{Stage: MeetingStageEnded, Flush: true, Reason: reason,
		NotifyOwner: reason == MeetingEndDenied})
}

// joinEndReason classifies a failed join flow into the closed reason set.
func joinEndReason(err error) string {
	switch {
	case errors.Is(err, ErrMeetingDenied):
		return MeetingEndDenied
	case errors.Is(err, errMeetingAdmitTimeout):
		return MeetingEndAdmitTimeout
	case errors.Is(err, errMeetingStopped):
		return MeetingEndStoppedByUser
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "join_failed"
}

// Denial probes match only explicit refusal wording ("can't join this call"
// is deliberately excluded: Meet also shows it for removal and policy blocks). An unknown or unreadable
// page keeps waiting, so a miss degrades to admit_timeout, never a false denial.
// Selectors are English-only and untuned against live pages.
const (
	meetDeniedJS = `(function(){` +
		`var t=(document.body&&document.body.innerText||'').toLowerCase();` +
		`return t.indexOf('denied your request to join')>=0;` +
		`})()`
	teamsDeniedJS = `(function(){` +
		`var t=(document.body&&document.body.innerText||'').toLowerCase();` +
		`return t.indexOf('denied access to the meeting')>=0||t.indexOf('your request to join was declined')>=0;` +
		`})()`
)

func meetingDenied(br meetPage, js string) bool {
	v, err := br.Evaluate(js)
	b, ok := v.(bool)
	return err == nil && ok && b
}
