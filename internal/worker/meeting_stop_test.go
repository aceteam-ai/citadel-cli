package worker

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
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
	if _, isHuddleWatched := r.watchHuddleCancellation(t.Context(), &Job{Type: JobTypeMeetingJoin}); isHuddleWatched() {
		t.Fatal("meeting still uses the hard huddle cancel watch")
	}
}
