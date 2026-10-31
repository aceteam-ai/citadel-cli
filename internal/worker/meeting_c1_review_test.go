package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/meetingpolicy"
)

// A stale-pending reclaim redelivers with a delivery count above 1. It must
// never rejoin: it fails terminally without running the handler.
func TestMeetingRedeliveryNeverRejoins(t *testing.T) {
	meetingTestEnvironment(t)
	var called atomic.Bool
	h := meetingTimingHandler{profile: meetingpolicy.Default(), run: func(context.Context, *Job, StreamWriter) (*JobResult, error) {
		called.Store(true)
		return &JobResult{Status: JobStatusSuccess}, nil
	}}
	source := NewMockJobSource("fake", nil)
	r := NewRunner(source, []JobHandler{h}, RunnerConfig{})
	job := &Job{ID: "synthetic", Type: JobTypeMeetingJoin, Metadata: JobMetadata{Attempts: 2, MaxAttempts: 3}}
	stream := &MockStreamWriter{}
	r.executeJob(context.Background(), job, stream, time.Now(), false, 0)
	if called.Load() || len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 {
		t.Fatal("redelivered meeting rejoined or was retried")
	}
	first := &Job{ID: "fresh", Type: JobTypeMeetingJoin, Metadata: JobMetadata{Attempts: 1, MaxAttempts: 3}}
	r.executeJob(context.Background(), first, &MockStreamWriter{}, time.Now(), false, 0)
	if !called.Load() {
		t.Fatal("first delivery was refused")
	}
}

// An ordinary job abandoned by its watchdog keeps the pre-#1505 contract: the
// leaked handler goroutine holds no execution ticket, so it can neither
// suppress STALL nor force a STUCK restart while a meeting is live.
func TestOrdinaryAbandonedHandlerReleasesTicket(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	h := uncooperativeHandler{block: block}
	state := NewWorkerState()
	r := NewRunner(NewMockJobSource("fake", nil), []JobHandler{h}, RunnerConfig{State: state})
	job := &Job{ID: "orphan", Type: "SHELL_COMMAND", Payload: map[string]any{"timeout_ms": 20}}
	r.executeJob(context.Background(), job, &MockStreamWriter{}, time.Now(), false, 0)
	if tracked, _ := state.trackedExecutionState(time.Now().Add(24*time.Hour), 5*time.Hour); tracked {
		t.Fatal("abandoned ordinary handler retained an execution ticket")
	}
}

// uncooperativeHandler ignores cancellation, like a wedged legacy handler.
type uncooperativeHandler struct{ block chan struct{} }

func (uncooperativeHandler) CanHandle(t string) bool { return t == "SHELL_COMMAND" }
func (h uncooperativeHandler) Execute(context.Context, *Job, StreamWriter) (*JobResult, error) {
	<-h.block
	return &JobResult{Status: JobStatusSuccess}, nil
}
