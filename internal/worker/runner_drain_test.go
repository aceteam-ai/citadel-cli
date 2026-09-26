package worker

import (
	"context"
	"sync"
	"testing"
	"time"
)

// blockingHandler blocks in Execute until release is closed, letting tests
// observe the runner's in-flight job count and drain behavior.
type blockingHandler struct {
	jobType string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

// latePollSource models a source that returns a job at the same instant its
// poll context is cancelled. The job is already source-claimed at that point.
type latePollSource struct {
	*MockJobSource
	started  chan struct{}
	returned bool
}

func (s *latePollSource) Next(ctx context.Context) (*Job, error) {
	if !s.returned {
		close(s.started)
		<-ctx.Done()
		s.returned = true
		return &Job{ID: "late", Type: "BLOCK", Payload: map[string]any{}}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func newBlockingHandler(jobType string) *blockingHandler {
	return &blockingHandler{
		jobType: jobType,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (h *blockingHandler) CanHandle(jobType string) bool { return h.jobType == jobType }

func (h *blockingHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	h.once.Do(func() { close(h.started) })
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	return &JobResult{Status: JobStatusSuccess, Output: map[string]any{"ok": true}}, nil
}

func TestRunnerActiveJobsAndDrain(t *testing.T) {
	handler := newBlockingHandler("BLOCK")
	job := &Job{ID: "j1", Type: "BLOCK", Payload: map[string]any{}}
	// Two jobs so we can confirm draining stops the second from starting.
	source := NewMockJobSource("test", []*Job{job, {ID: "j2", Type: "BLOCK", Payload: map[string]any{}}})

	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{
		WorkerID:       "w",
		MaxConcurrency: 1,
		ActivityFn:     func(string, string) {}, // silence
	})

	if runner.ActiveJobs() != 0 {
		t.Fatalf("expected 0 active jobs before start, got %d", runner.ActiveJobs())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = runner.Run(ctx); close(done) }()

	// Wait for the first job to enter the handler.
	select {
	case <-handler.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first job never started")
	}

	if got := runner.ActiveJobs(); got != 1 {
		t.Errorf("expected 1 active job, got %d", got)
	}

	// Drain: no new jobs should be fetched. Release the in-flight job.
	runner.Drain()
	close(handler.release)

	// Active jobs should return to 0 and stay there (j2 must never start).
	deadline := time.Now().Add(2 * time.Second)
	for runner.ActiveJobs() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("active jobs did not return to 0, got %d", runner.ActiveJobs())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Give the loop a moment; ensure j2 was not picked up while draining.
	time.Sleep(100 * time.Millisecond)
	if got := runner.ActiveJobs(); got != 0 {
		t.Errorf("expected runner to stay idle while draining, got %d active", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not shut down")
	}
}

func TestRunnerDrainScopesRespectOwners(t *testing.T) {
	runner := NewRunner(NewMockJobSource("test", nil), nil, RunnerConfig{})
	first := runner.BeginDrain()
	second := runner.BeginDrain()
	first()
	first() // release is idempotent
	if !runner.IsDraining() {
		t.Fatal("first release cleared the second scope")
	}
	runner.Drain()
	second()
	if !runner.IsDraining() {
		t.Fatal("releasing scopes cleared permanent shutdown drain")
	}
}

func TestRunnerDrainCountsJobReturnedByCancelledPoll(t *testing.T) {
	source := &latePollSource{MockJobSource: NewMockJobSource("test", nil), started: make(chan struct{})}
	handler := newBlockingHandler("BLOCK")
	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{
		WorkerID: "w", ActivityFn: func(string, string) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = runner.Run(ctx); close(done) }()
	select {
	case <-source.started:
	case <-time.After(2 * time.Second):
		t.Fatal("source poll did not start")
	}
	release := runner.BeginDrain()
	if got := runner.ActiveJobs(); got != 1 {
		t.Fatalf("source-claimed job awaiting release: ActiveJobs=%d, want 1", got)
	}
	select {
	case <-handler.started:
		t.Fatal("job started while drain scope was held")
	default:
	}
	release()
	select {
	case <-handler.started:
	case <-time.After(2 * time.Second):
		t.Fatal("pending job did not resume after release")
	}
	close(handler.release)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not stop")
	}
}
