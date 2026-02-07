package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/usage"
)

// MockJobSource is a test implementation of JobSource.
type MockJobSource struct {
	name           string
	jobs           []*Job
	jobIndex       int
	acked          []*Job
	nacked         []*Job
	connected      bool
	closed         bool
	mu             sync.Mutex
	cancelledJobs  map[string]bool
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

func (m *MockJobSource) IsJobCancelled(ctx context.Context, jobID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancelledJobs == nil {
		return false
	}
	return m.cancelledJobs[jobID]
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
	started   bool
	chunks    []string
	ended     bool
	errored   bool
	cancelled bool
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

func (m *MockStreamWriter) WriteCancelled(reason string) error {
	m.cancelled = true
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

	factory := func(job *Job) StreamWriter {
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
	if err := sw.WriteCancelled("test reason"); err != nil {
		t.Errorf("WriteCancelled error = %v, want nil", err)
	}
}

func TestRunnerCancelledJobSkipsHandler(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "TEST_JOB", Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	source.cancelledJobs = map[string]bool{"job-1": true}

	handler := NewMockJobHandler("TEST_JOB", false)
	handlers := []JobHandler{handler}

	mockStream := &MockStreamWriter{}
	config := RunnerConfig{WorkerID: "test-worker"}
	runner := NewRunner(source, handlers, config)
	runner.WithStreamWriterFactory(func(job *Job) StreamWriter {
		return mockStream
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	// Handler should NOT have executed
	if len(handler.ExecutedJobs()) != 0 {
		t.Errorf("Expected 0 executed jobs, got %d", len(handler.ExecutedJobs()))
	}
	// Job should be acked (removed from queue)
	if len(source.AckedJobs()) != 1 {
		t.Errorf("Expected 1 acked job, got %d", len(source.AckedJobs()))
	}
	// Cancelled event should have been written
	if !mockStream.cancelled {
		t.Error("Expected WriteCancelled to be called")
	}
}

// TestRunnerActivityCallback tests that the activity callback is invoked during job processing
func TestRunnerActivityCallback(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "TEST_JOB", Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("TEST_JOB", false)
	handlers := []JobHandler{handler}

	// Track activity messages
	var activityMessages []struct {
		level string
		msg   string
	}
	var activityMu sync.Mutex

	config := RunnerConfig{
		WorkerID: "test-worker",
		ActivityFn: func(level, msg string) {
			activityMu.Lock()
			activityMessages = append(activityMessages, struct {
				level string
				msg   string
			}{level, msg})
			activityMu.Unlock()
		},
	}

	runner := NewRunner(source, handlers, config)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runner.Run(ctx)

	// Check that activity callback was called
	activityMu.Lock()
	msgCount := len(activityMessages)
	activityMu.Unlock()

	if msgCount == 0 {
		t.Error("Activity callback was not called")
	}

	// Check that we have at least the expected message types
	hasInfoMessage := false
	hasSuccessMessage := false

	activityMu.Lock()
	for _, m := range activityMessages {
		if m.level == "info" {
			hasInfoMessage = true
		}
		if m.level == "success" {
			hasSuccessMessage = true
		}
	}
	activityMu.Unlock()

	if !hasInfoMessage {
		t.Error("Expected at least one 'info' level activity message")
	}
	if !hasSuccessMessage {
		t.Error("Expected at least one 'success' level activity message for completed job")
	}
}

// TestRunnerJobRecordCallback tests that the job record callback is invoked on job completion
func TestRunnerJobRecordCallback(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "TEST_JOB", Payload: map[string]any{}},
		{ID: "job-2", Type: "FAIL_JOB", Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	successHandler := NewMockJobHandler("TEST_JOB", false)
	failHandler := NewMockJobHandler("FAIL_JOB", true)
	handlers := []JobHandler{successHandler, failHandler}

	// Track job records
	var jobRecords []usage.UsageRecord
	var recordMu sync.Mutex

	config := RunnerConfig{
		WorkerID: "test-worker",
		JobRecordFn: func(record usage.UsageRecord) {
			recordMu.Lock()
			jobRecords = append(jobRecords, record)
			recordMu.Unlock()
		},
	}

	runner := NewRunner(source, handlers, config)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runner.Run(ctx)

	// Check that job records were created
	recordMu.Lock()
	recordCount := len(jobRecords)
	recordMu.Unlock()

	if recordCount != 2 {
		t.Errorf("Expected 2 job records, got %d", recordCount)
	}

	// Check success record
	recordMu.Lock()
	var successRecord, failRecord usage.UsageRecord
	for _, r := range jobRecords {
		if r.JobID == "job-1" {
			successRecord = r
		}
		if r.JobID == "job-2" {
			failRecord = r
		}
	}
	recordMu.Unlock()

	if successRecord.Status != "success" {
		t.Errorf("Success job status = %s, want success", successRecord.Status)
	}
	if successRecord.ErrorMessage != "" {
		t.Errorf("Success job error = %q, want empty", successRecord.ErrorMessage)
	}
	if successRecord.StartedAt.IsZero() {
		t.Error("Success job started time should not be zero")
	}
	if successRecord.CompletedAt.IsZero() {
		t.Error("Success job completed time should not be zero")
	}
	if successRecord.DurationMs < 0 {
		t.Error("Success job duration should not be negative")
	}

	// Check failed record
	if failRecord.Status != "failed" {
		t.Errorf("Failed job status = %s, want failed", failRecord.Status)
	}
	if failRecord.ErrorMessage == "" {
		t.Error("Failed job error should not be empty")
	}
}

// TestRunnerActivityCallbackOnError tests that activity callback logs errors
func TestRunnerActivityCallbackOnError(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "UNKNOWN_JOB", Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("OTHER_JOB", false) // Doesn't handle UNKNOWN_JOB
	handlers := []JobHandler{handler}

	var hasErrorMessage bool
	var messageMu sync.Mutex

	config := RunnerConfig{
		WorkerID: "test-worker",
		ActivityFn: func(level, msg string) {
			if level == "error" {
				messageMu.Lock()
				hasErrorMessage = true
				messageMu.Unlock()
			}
		},
	}

	runner := NewRunner(source, handlers, config)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runner.Run(ctx)

	messageMu.Lock()
	hadError := hasErrorMessage
	messageMu.Unlock()

	if !hadError {
		t.Error("Expected error activity message when no handler found")
	}
}

// TestRunnerConfigActivityFnSetOnNew tests that ActivityFn is set from config
func TestRunnerConfigActivityFnSetOnNew(t *testing.T) {
	source := NewMockJobSource("test", nil)

	activityCalled := false
	config := RunnerConfig{
		WorkerID: "test-worker",
		ActivityFn: func(level, msg string) {
			activityCalled = true
		},
	}

	runner := NewRunner(source, nil, config)

	if runner.activityFn == nil {
		t.Error("activityFn should be set from config")
	}

	// Verify the function is the one we passed
	runner.activityFn("info", "test")
	if !activityCalled {
		t.Error("activityFn should invoke our callback")
	}
}

// TestRunnerConfigJobRecordFnSetOnNew tests that JobRecordFn is set from config
func TestRunnerConfigJobRecordFnSetOnNew(t *testing.T) {
	source := NewMockJobSource("test", nil)

	recordCalled := false
	config := RunnerConfig{
		WorkerID: "test-worker",
		JobRecordFn: func(record usage.UsageRecord) {
			recordCalled = true
		},
	}

	runner := NewRunner(source, nil, config)

	if runner.jobRecordFn == nil {
		t.Error("jobRecordFn should be set from config")
	}

	// Verify the function is the one we passed
	runner.jobRecordFn(usage.UsageRecord{JobID: "test-id", JobType: "test-type", Status: "success"})
	if !recordCalled {
		t.Error("jobRecordFn should invoke our callback")
	}
}

// TestRunnerLogWithoutActivityFn tests log method falls back to stdout when no callback
func TestRunnerLogWithoutActivityFn(t *testing.T) {
	source := NewMockJobSource("test", nil)
	config := RunnerConfig{
		WorkerID: "test-worker",
		// No ActivityFn set - should use default stdout/stderr
	}

	runner := NewRunner(source, nil, config)

	// This should not panic even without ActivityFn
	runner.log("info", "test message %s", "arg")
	runner.log("error", "error message")
	runner.log("warning", "warning message")
}

// TestRunnerRecordJobWithoutCallback tests recordJob is no-op without callback
func TestRunnerRecordJobWithoutCallback(t *testing.T) {
	source := NewMockJobSource("test", nil)
	config := RunnerConfig{
		WorkerID: "test-worker",
		// No JobRecordFn set
	}

	runner := NewRunner(source, nil, config)

	// This should not panic even without JobRecordFn
	runner.recordJob(usage.UsageRecord{JobID: "test-id", JobType: "test-type", Status: "success"})
}
