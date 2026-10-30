package worker

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/meetingpolicy"
)

type meetingTimingHandler struct {
	profile meetingpolicy.Profile
	run     func(context.Context, *Job, StreamWriter) (*JobResult, error)
}

func (h meetingTimingHandler) CanHandle(kind string) bool { return kind == JobTypeMeetingJoin }
func (h meetingTimingHandler) MeetingProfile() (meetingpolicy.Profile, error) {
	return h.profile, h.profile.Validate()
}
func (h meetingTimingHandler) Execute(c context.Context, j *Job, w StreamWriter) (*JobResult, error) {
	if h.run != nil {
		return h.run(c, j, w)
	}
	return &JobResult{Status: JobStatusSuccess}, nil
}
func meetingTestEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv(jobTimeoutLongEnvVar, "")
	t.Setenv(selfHealStuckEnvVar, "")
	t.Setenv(selfHealEnabledEnvVar, "true")
}
func TestMeetingWorkerUsesSeparateFiniteProfile(t *testing.T) {
	meetingTestEnvironment(t)
	h := meetingTimingHandler{profile: meetingpolicy.Default()}
	r := NewRunner(NewMockJobSource("fake", nil), []JobHandler{h}, RunnerConfig{})
	for _, tc := range []struct {
		payload map[string]any
		want    time.Duration
	}{{nil, 29280 * time.Second}, {map[string]any{"admit_timeout_seconds": "600"}, 29700 * time.Second}, {map[string]any{"admit_timeout_seconds": 600.0, "max_duration_seconds": 21600.0, "timeout_ms": "36900000"}, 36900 * time.Second}} {
		c, d, bounded, err := r.resolveExecutionBudget(context.Background(), h, &Job{Type: JobTypeMeetingJoin, Payload: tc.payload})
		if err != nil || !bounded || d != tc.want || jobs.MeetingCleanupFrom(c) == nil {
			t.Fatal(d, bounded, err)
		}
	}
	_, d, ok, e := r.resolveExecutionBudget(context.Background(), h, &Job{Type: JobTypeCobrowse})
	if e != nil || !ok || d != 4*time.Hour {
		t.Fatal("unrelated long tier changed")
	}
	for _, payload := range []map[string]any{{"admit_timeout_seconds": ""}, {"admit_timeout_seconds": "601"}, {"admit_timeout_seconds": 1.5}, {"max_duration_seconds": math.Inf(1)}, {"max_duration_seconds": "59"}, {"timeout_ms": "29279999"}, {"timeout_ms": 29280000.5}, {"timeout_ms": nil}} {
		if _, _, _, err := r.resolveExecutionBudget(context.Background(), h, &Job{Type: JobTypeMeetingJoin, Payload: payload}); err == nil {
			t.Fatal("bad budget accepted", payload)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, _, _, err := r.resolveExecutionBudget(ctx, h, &Job{Type: JobTypeMeetingJoin}); err == nil {
		t.Fatal("parent authority extended")
	}
}
func TestMeetingWorkerReadinessRespectsExplicitOperatorLimits(t *testing.T) {
	meetingTestEnvironment(t)
	adapter := NewLegacyHandlerAdapter(JobTypeMeetingJoin, &jobs.MeetingJoinHandler{})
	if !adapter.CanHandle(JobTypeMeetingJoin) {
		t.Fatal("ratified default not ready")
	}
	for _, pair := range [][2]string{{jobTimeoutLongEnvVar, "14400"}, {selfHealStuckEnvVar, "18000"}, {selfHealStuckEnvVar, "0"}, {selfHealEnabledEnvVar, "false"}, {jobTimeoutLongEnvVar, "garbage"}} {
		t.Run(pair[0]+pair[1], func(t *testing.T) {
			t.Setenv(pair[0], pair[1])
			if adapter.CanHandle(JobTypeMeetingJoin) || adapter.RefusalFor(JobTypeMeetingJoin) == nil {
				t.Fatal("incompatible profile advertised")
			}
		})
	}
	invalid := NewLegacyHandlerAdapter(JobTypeMeetingJoin, &jobs.MeetingJoinHandler{TimingError: errors.New("synthetic malformed config")})
	if invalid.CanHandle(JobTypeMeetingJoin) {
		t.Fatal("malformed config advertised")
	}
}
func TestMeetingWorkerInvalidBudgetNeverRunsHandlerOrRetries(t *testing.T) {
	meetingTestEnvironment(t)
	var called atomic.Bool
	h := meetingTimingHandler{profile: meetingpolicy.Default(), run: func(context.Context, *Job, StreamWriter) (*JobResult, error) { called.Store(true); return nil, nil }}
	source := NewMockJobSource("fake", nil)
	r := NewRunner(source, []JobHandler{h}, RunnerConfig{})
	job := &Job{ID: "synthetic", Type: JobTypeMeetingJoin, Payload: map[string]any{"timeout_ms": "1"}}
	if r.executeJob(context.Background(), job, &MockStreamWriter{}, time.Now(), false, 0) || called.Load() || len(source.NackedJobs()) != 0 || len(source.FailedJobs()) != 1 {
		t.Fatal("invalid profile reached native work or automatic retry")
	}
}
func TestMeetingCancellationDoesNotUseHuddleTerminalAuthority(t *testing.T) {
	meetingTestEnvironment(t)
	source := NewMockJobSource("fake", nil)
	source.cancelledJobs = map[string]bool{"synthetic": true}
	h := meetingTimingHandler{profile: meetingpolicy.Default(), run: func(c context.Context, _ *Job, _ StreamWriter) (*JobResult, error) {
		<-c.Done()
		return nil, jobs.ErrHuddleTerminalOwned
	}}
	r := NewRunner(source, []JobHandler{h}, RunnerConfig{})
	r.huddleCancelPoll = time.Millisecond
	stream := &meetingCancellationWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.executeJob(ctx, &Job{ID: "synthetic", Type: JobTypeMeetingJoin}, stream, time.Now(), false, 0)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("meeting cancel marker was not observed")
	}
	if stream.message != "Meeting cancelled" || !stream.cancelled || len(source.AckedJobs()) != 1 || len(source.NackedJobs()) != 0 || len(source.FailedJobs()) != 0 {
		t.Fatal("meeting cancellation inherited huddle lifecycle or retried")
	}
}
func TestMeetingUnknownTeardownRetainsCustodyWithoutAckNackOrFail(t *testing.T) {
	meetingTestEnvironment(t)
	h := meetingTimingHandler{profile: meetingpolicy.Default(), run: func(context.Context, *Job, StreamWriter) (*JobResult, error) {
		return nil, jobs.ErrMeetingTeardownUnconfirmed
	}}
	source := NewMockJobSource("fake", nil)
	state := NewWorkerState()
	r := NewRunner(source, []JobHandler{h}, RunnerConfig{State: state})
	r.executeJob(context.Background(), &Job{ID: "synthetic", Type: JobTypeMeetingJoin}, &MockStreamWriter{}, time.Now(), false, 0)
	if len(source.AckedJobs())+len(source.NackedJobs())+len(source.FailedJobs()) != 0 {
		t.Fatal("uncertain teardown released source")
	}
	tracked, stuck := state.trackedExecutionState(time.Now().Add(24*time.Hour), 5*time.Hour)
	if !tracked || !stuck {
		t.Fatal("uncertainty escaped recovery tracking")
	}
}
func TestMeetingUncooperativeDeadlineRetainsActualHandler(t *testing.T) {
	state := NewWorkerState()
	r := NewRunner(NewMockJobSource("fake", nil), nil, RunnerConfig{State: state})
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	h := meetingTimingHandler{profile: meetingpolicy.Default(), run: func(context.Context, *Job, StreamWriter) (*JobResult, error) {
		close(started)
		<-release
		return &JobResult{Status: JobStatusSuccess}, nil
	}}
	b, _ := meetingpolicy.Default().Maximum()
	ticket := state.beginExecution(JobTypeMeetingJoin, time.Now(), b.StuckAfter())
	ctx := context.WithValue(context.Background(), executionTicketKey{}, ticket)
	done := make(chan error, 1)
	go func() {
		_, err := r.executeWithDeadline(ctx, h, &Job{ID: "synthetic", Type: JobTypeMeetingJoin}, &MockStreamWriter{}, 10*time.Millisecond)
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		t.Fatal("deadline freed actual handler", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	err := <-done
	var deadline *deadlineExceededError
	if !errors.As(err, &deadline) {
		t.Fatal("late success masked deadline", err)
	}
	tracked, _ := state.trackedExecutionState(time.Now(), 5*time.Hour)
	if !tracked {
		t.Fatal("terminal publication custody lost")
	}
	ticket.pipelineFinished()
	tracked, _ = state.trackedExecutionState(time.Now(), 5*time.Hour)
	if tracked {
		t.Fatal("finished execution leaked")
	}
}
func TestMeetingExecutionSpecificStuckCannotHideAnotherExecution(t *testing.T) {
	anchor := time.Now()
	s := NewWorkerState()
	b, _ := meetingpolicy.Default().Maximum()
	meeting := s.beginExecution(JobTypeMeetingJoin, anchor, b.StuckAfter())
	other := s.beginExecution(JobTypeShellCommand, anchor.Add(10*time.Minute), 0)
	m := newTestMonitor(s, false)
	m.startedAt = anchor.Add(-time.Hour)
	if reason, stuck := m.check(anchor.Add(5 * time.Hour)); stuck {
		t.Fatal("legitimate meeting restarted at five hours", reason)
	}
	if _, stuck := m.check(anchor.Add(5*time.Hour + 10*time.Minute)); stuck {
		t.Fatal("ordinary strict boundary changed")
	}
	if _, stuck := m.check(anchor.Add(5*time.Hour + 10*time.Minute + time.Nanosecond)); !stuck {
		t.Fatal("long meeting hid an ordinary stuck execution")
	}
	other.handlerFinished(false)
	other.pipelineFinished()
	for i := 0; i < 3; i++ {
		short := s.beginExecution(JobTypeShellCommand, anchor.Add(6*time.Hour), 0)
		short.handlerFinished(false)
		short.pipelineFinished()
	}
	if _, stuck := m.check(anchor.Add(b.StuckAfter() - time.Nanosecond)); stuck {
		t.Fatal("meeting recovered before E+K")
	}
	if _, stuck := m.check(anchor.Add(b.StuckAfter())); !stuck {
		t.Fatal("meeting never reached E+K")
	}
	meeting.handlerFinished(false)
	if _, stuck := m.check(anchor.Add(b.StuckAfter())); !stuck {
		t.Fatal("publication dropped the execution timer")
	}
	meeting.pipelineFinished()
	if tracked, _ := s.trackedExecutionState(anchor.Add(24*time.Hour), 5*time.Hour); tracked {
		t.Fatal("ticket did not release after both exits")
	}
}
func TestMeetingStuckFencesIntakeBeforeExistingRestart(t *testing.T) {
	m := newTestMonitor(NewWorkerState(), false)
	var order []string
	m.SetRestartFence(func() { order = append(order, "fence") })
	m.onWedge = func(string) { order = append(order, "restart") }
	if !m.handleWedge("synthetic stuck", time.Now()) || len(order) != 2 || order[0] != "fence" || order[1] != "restart" {
		t.Fatal(order)
	}
}

type meetingStartWriter struct {
	MockStreamWriter
	delay time.Duration
}

func (w *meetingStartWriter) WriteStart(s string) error {
	time.Sleep(w.delay)
	return w.MockStreamWriter.WriteStart(s)
}
func TestMeetingWorkerExecutionDeadlineDoesNotRenewAfterStartPublication(t *testing.T) {
	meetingTestEnvironment(t)
	state := NewWorkerState()
	var checked bool
	h := meetingTimingHandler{profile: meetingpolicy.Default(), run: func(c context.Context, _ *Job, _ StreamWriter) (*JobResult, error) {
		ticket, _ := c.Value(executionTicketKey{}).(*executionTicket)
		b, _ := c.Value(meetingBudgetKey{}).(meetingpolicy.Budget)
		deadline, ok := c.Deadline()
		if !ok || ticket == nil || !deadline.Equal(ticket.started.Add(b.Execution())) {
			t.Error("execution budget renewed after publication")
		}
		checked = true
		return &JobResult{Status: JobStatusSuccess}, nil
	}}
	r := NewRunner(NewMockJobSource("fake", nil), []JobHandler{h}, RunnerConfig{State: state})
	writer := &meetingStartWriter{delay: 10 * time.Millisecond}
	r.executeJob(context.Background(), &Job{ID: "synthetic", Type: JobTypeMeetingJoin}, writer, time.Now(), false, 0)
	if !checked {
		t.Fatal("fake handler not reached")
	}
}

type meetingBlockedPublication struct {
	MockStreamWriter
	entered, release chan struct{}
}

func (w *meetingBlockedPublication) WriteEnd(out map[string]any) error {
	close(w.entered)
	<-w.release
	return w.MockStreamWriter.WriteEnd(out)
}
func TestMeetingWorkerPublicationRemainsInsideStuckCustody(t *testing.T) {
	meetingTestEnvironment(t)
	state := NewWorkerState()
	h := meetingTimingHandler{profile: meetingpolicy.Default()}
	source := NewMockJobSource("fake", nil)
	r := NewRunner(source, []JobHandler{h}, RunnerConfig{State: state})
	w := &meetingBlockedPublication{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.executeJob(context.Background(), &Job{ID: "synthetic", Type: JobTypeMeetingJoin}, w, time.Now(), false, 0)
	}()
	<-w.entered
	tracked, stuck := state.trackedExecutionState(time.Now().Add(24*time.Hour), 5*time.Hour)
	close(w.release)
	<-done
	if !tracked || !stuck {
		t.Fatal("blocked terminal publication dropped recovery custody")
	}
	if tracked, _ := state.trackedExecutionState(time.Now(), 5*time.Hour); tracked {
		t.Fatal("publication completion leaked custody")
	}
}

type meetingCancellationWriter struct {
	MockStreamWriter
	message string
}

func (w *meetingCancellationWriter) WriteCancelled(message string) error {
	w.message = message
	return w.MockStreamWriter.WriteCancelled(message)
}
func TestMeetingRecoveryFenceStopsPickupWithoutWaitingPoll(t *testing.T) {
	r := NewRunner(NewMockJobSource("fake", nil), nil, RunnerConfig{})
	var cancelled atomic.Bool
	r.pollCancel = func() { cancelled.Store(true) }
	r.pollDone = make(chan struct{})
	done := make(chan struct{})
	defer close(r.pollDone)
	go func() { r.FenceIntakeForRecovery(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recovery fence waited for wedged poll")
	}
	if !r.IsDraining() || !cancelled.Load() {
		t.Fatal("recovery did not fence and cancel pickup")
	}
}
