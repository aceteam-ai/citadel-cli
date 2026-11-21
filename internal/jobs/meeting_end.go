package jobs

import (
	"math"
	"time"
)

// A scheduled bot can be admitted before the other participants arrive.
// #7074 permits ending an otherwise empty room after fifteen minutes, or once
// another participant has been observed and everyone else subsequently leaves.
const meetingNobodyJoinedGrace = 15 * time.Minute

// meetingEndTracker belongs to one recording loop, never the handler or node.
// Its time input makes boundary tests independent of a real browser or wait.
type meetingEndTracker struct {
	recordingStartedAt time.Time
	seenOther          bool
}

func (s *meetingEndTracker) check(page meetPage, now time.Time) (string, bool) {
	// An explicit call-ended signal remains authoritative during the grace.
	if v, err := page.Evaluate(meetIsEndedJS); err == nil {
		if ended, ok := v.(bool); ok && ended {
			return "call_ended", true
		}
	}
	v, err := page.Evaluate(meetParticipantCountJS)
	if err != nil {
		return "", false
	}
	other, ok := observedMeetingParticipants(v)
	if !ok {
		return "", false // An unreadable toolbar is not evidence of an empty room.
	}
	if other {
		s.seenOther = true
		return "", false
	}
	if s.seenOther {
		return "alone_in_call", true
	}
	if s.recordingStartedAt.IsZero() || now.Sub(s.recordingStartedAt) < meetingNobodyJoinedGrace {
		return "", false
	}
	return "nobody_joined", true
}

// Classify without narrowing a CDP number to a machine int: malformed or
// fractional observations cannot masquerade as a participant leaving.
func observedMeetingParticipants(v any) (other, known bool) {
	switch n := v.(type) {
	case int:
		return n > 1, n >= 0
	case int64:
		return n > 1, n >= 0
	case float64:
		return n > 1, n >= 0 && !math.IsInf(n, 0) && n == math.Trunc(n)
	default:
		return false, false
	}
}
