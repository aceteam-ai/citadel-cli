package worker

import (
	"context"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/meetingpolicy"
)

// The production adapter forwards the meeting handler's cooperative Stop, so
// a Stop marker reaches the handler gracefully; the meeting keeps its own
// budget and long-session lane rather than a declared heavy lane or tier.
func TestMeetingAdapterDeclaresCooperativeStop(t *testing.T) {
	adapter := NewLegacyHandlerAdapter(JobTypeMeetingJoin, jobs.NewMeetingJoinHandler(t.TempDir()))
	r := NewRunner(NewMockJobSource("fake", nil), []JobHandler{adapter}, RunnerConfig{})
	c := r.capabilitiesFor(JobTypeMeetingJoin)
	if !c.CooperativeCancel || c.Lane != jobs.LaneNormal || c.Deadline != jobs.DeadlineUnset {
		t.Fatalf("meeting capabilities %+v", c)
	}
}

// A Stop marker during recording must reach MEETING_JOIN only as the graceful
// cooperative flag. The hard huddle watch is configured to fire first: if it
// were restored for meetings, the handler's context would be cancelled and
// the stopped (transcribed) result would be lost.
func TestMeetingStopIsCooperativeNotHardCancel(t *testing.T) {
	meetingTestEnvironment(t)
	source := NewMockJobSource("fake", nil)
	source.cancelledJobs = map[string]bool{"synthetic": true}
	h := coopMeetingHandler{meetingTimingHandler{profile: meetingpolicy.Default(), run: func(c context.Context, _ *Job, _ StreamWriter) (*JobResult, error) {
		requested := jobs.CancelRequestedFrom(c)
		for requested == nil || !requested() {
			if c.Err() != nil {
				return nil, c.Err()
			}
			time.Sleep(time.Millisecond)
		}
		if c.Err() != nil {
			return nil, c.Err()
		}
		return &JobResult{Status: JobStatusSuccess, Output: map[string]any{"status": "stopped"}}, nil
	}}}
	r := NewRunner(source, []JobHandler{h}, RunnerConfig{})
	r.huddleCancelPoll = time.Millisecond
	r.coopCancelPoll = 30 * time.Millisecond
	stream := &meetingCancellationWriter{}
	r.executeJob(context.Background(), &Job{ID: "synthetic", Type: JobTypeMeetingJoin}, stream, time.Now(), false, 0)
	if stream.cancelled || stream.message != "" || len(source.AckedJobs()) != 1 || len(source.FailedJobs()) != 0 {
		t.Fatalf("stop was a hard cancel: cancelled=%v msg=%q acked=%d failed=%d", stream.cancelled, stream.message, len(source.AckedJobs()), len(source.FailedJobs()))
	}
}
