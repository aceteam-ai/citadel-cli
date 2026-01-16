package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// MockJobSource is a test implementation of JobSource.
type MockJobSource struct {
	name      string
	jobs      []*Job
	jobIndex  int
	acked     []*Job
	nacked    []*Job
	connected bool
	closed    bool
	mu        sync.Mutex
}

func NewMockJobSource(name string, jobs []*Job) *MockJobSource {
	return &MockJobSource{
		name:   name,
		jobs:   jobs,
		acked:  make([]*Job, 0),
		nacked: make([]*Job, 0),
	}
}

func (m *MockJobSource) Name() string {
	return m.name
}

func (m *MockJobSource) Connect(ctx context.Context) error {
	m.connected = true
	return nil
}

func (m *MockJobSource) Next(ctx context.Context) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		if m.jobIndex >= len(m.jobs) {
			return nil, nil
		}
		job := m.jobs[m.jobIndex]
		m.jobIndex++
		return job, nil
	}
}

func (m *MockJobSource) Ack(ctx context.Context, job *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acked = append(m.acked, job)
	return nil
}

func (m *MockJobSource) Nack(ctx context.Context, job *Job, err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nacked = append(m.nacked, job)
	return nil
}

func (m *MockJobSource) Close() error {
	m.closed = true
	return nil
}

func (m *MockJobSource) AckedJobs() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acked
}

func (m *MockJobSource) NackedJobs() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nacked
}

// MockJobHandler is a test implementation of JobHandler.
type MockJobHandler struct {
	jobType    string
	shouldFail bool
	executed   []*Job
	mu         sync.Mutex
}

func NewMockJobHandler(jobType string, shouldFail bool) *MockJobHandler {
	return &MockJobHandler{
		jobType:    jobType,
		shouldFail: shouldFail,
		executed:   make([]*Job, 0),
	}
}

func (m *MockJobHandler) CanHandle(jobType string) bool {
	return m.jobType == jobType
}

func (m *MockJobHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	m.mu.Lock()
	m.executed = append(m.executed, job)
	m.mu.Unlock()

	if m.shouldFail {
		return &JobResult{
			Status: JobStatusFailure,
			Error:  errors.New("mock handler failure"),
		}, errors.New("mock handler failure")
	}

	return &JobResult{
		Status: JobStatusSuccess,
		Output: map[string]any{"executed": true},
	}, nil
}

func (m *MockJobHandler) ExecutedJobs() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.executed
}

// MockStreamWriter is a test implementation of StreamWriter.
type MockStreamWriter struct {
	started bool
	chunks  []string
	ended   bool
	errored bool
}

func (m *MockStreamWriter) WriteStart(message string) error {
	m.started = true
	return nil
}

func (m *MockStreamWriter) WriteChunk(content string, index int) error {
	m.chunks = append(m.chunks, content)
	return nil
}

func (m *MockStreamWriter) WriteEnd(result map[string]any) error {
	m.ended = true
	return nil
}

func (m *MockStreamWriter) WriteError(err error, recoverable bool) error {
	m.errored = true
	return nil
}

func TestNewRunner(t *testing.T) {
	source := NewMockJobSource("test", nil)
	handlers := []JobHandler{NewMockJobHandler("TEST_JOB", false)}
	config := RunnerConfig{
		WorkerID: "test-worker",
		Verbose:  true,
	}

	runner := NewRunner(source, handlers, config)

	if runner == nil {
		t.Fatal("NewRunner returned nil")
	}
	if runner.source != source {
		t.Error("Runner source not set correctly")
	}
	if len(runner.handlers) != 1 {
		t.Errorf("Runner handlers count = %d, want 1", len(runner.handlers))
	}
	if runner.config.WorkerID != "test-worker" {
		t.Errorf("Runner config.WorkerID = %s, want test-worker", runner.config.WorkerID)
	}
}

func TestRunnerWithStreamWriterFactory(t *testing.T) {
	source := NewMockJobSource("test", nil)
	handlers := []JobHandler{}
	config := RunnerConfig{WorkerID: "test"}

	runner := NewRunner(source, handlers, config)

	factory := func(jobID string) StreamWriter {
		return &MockStreamWriter{}
	}

	result := runner.WithStreamWriterFactory(factory)

	if result != runner {
		t.Error("WithStreamWriterFactory should return the runner for chaining")
	}
	if runner.streamWriterFactory == nil {
		t.Error("streamWriterFactory should be set")
	}
}

func TestRunnerProcessesJobs(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "TEST_JOB", Payload: map[string]any{}},
		{ID: "job-2", Type: "TEST_JOB", Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("TEST_JOB", false)
	handlers := []JobHandler{handler}
	config := RunnerConfig{WorkerID: "test-worker"}

	runner := NewRunner(source, handlers, config)

	// Run with timeout context
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runner.Run(ctx)

	// Check that jobs were processed
	executed := handler.ExecutedJobs()
	if len(executed) != 2 {
		t.Errorf("Executed jobs = %d, want 2", len(executed))
	}

	// Check that jobs were acked
	acked := source.AckedJobs()
	if len(acked) != 2 {
		t.Errorf("Acked jobs = %d, want 2", len(acked))
	}

	// Check source was closed
	if !source.closed {
		t.Error("Source should be closed after Run")
	}
}

func TestRunnerNacksFailedJobs(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "FAIL_JOB", Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("FAIL_JOB", true) // This handler fails
	handlers := []JobHandler{handler}
	config := RunnerConfig{WorkerID: "test-worker"}

	runner := NewRunner(source, handlers, config)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runner.Run(ctx)

	// Check that job was executed
	executed := handler.ExecutedJobs()
	if len(executed) != 1 {
		t.Errorf("Executed jobs = %d, want 1", len(executed))
	}

	// Check that job was nacked (not acked)
	nacked := source.NackedJobs()
	if len(nacked) != 1 {
		t.Errorf("Nacked jobs = %d, want 1", len(nacked))
	}

	acked := source.AckedJobs()
	if len(acked) != 0 {
		t.Errorf("Acked jobs = %d, want 0", len(acked))
	}
}

func TestRunnerNoHandler(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "UNKNOWN_JOB", Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("OTHER_JOB", false) // Doesn't handle UNKNOWN_JOB
	handlers := []JobHandler{handler}
	config := RunnerConfig{WorkerID: "test-worker"}

	runner := NewRunner(source, handlers, config)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runner.Run(ctx)

	// Job should be nacked because no handler
	nacked := source.NackedJobs()
	if len(nacked) != 1 {
		t.Errorf("Nacked jobs = %d, want 1", len(nacked))
	}

	// Handler should not have executed anything
	executed := handler.ExecutedJobs()
	if len(executed) != 0 {
		t.Errorf("Executed jobs = %d, want 0", len(executed))
	}
}

func TestRunnerRegisterHandler(t *testing.T) {
	source := NewMockJobSource("test", nil)
	config := RunnerConfig{WorkerID: "test"}

	runner := NewRunner(source, nil, config)

	if len(runner.handlers) != 0 {
		t.Errorf("Initial handlers = %d, want 0", len(runner.handlers))
	}

	handler := NewMockJobHandler("TEST", false)
	runner.RegisterHandler(handler)

	if len(runner.handlers) != 1 {
		t.Errorf("After register handlers = %d, want 1", len(runner.handlers))
	}
}

func TestNoOpStreamWriter(t *testing.T) {
	sw := &NoOpStreamWriter{}

	// All methods should return nil and not panic
	if err := sw.WriteStart("test"); err != nil {
		t.Errorf("WriteStart error = %v, want nil", err)
	}
	if err := sw.WriteChunk("chunk", 0); err != nil {
		t.Errorf("WriteChunk error = %v, want nil", err)
	}
	if err := sw.WriteEnd(nil); err != nil {
		t.Errorf("WriteEnd error = %v, want nil", err)
	}
	if err := sw.WriteError(errors.New("test"), false); err != nil {
		t.Errorf("WriteError error = %v, want nil", err)
	}
}
