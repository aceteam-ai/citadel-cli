package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/usage"
)

// MockJobSource is a test implementation of JobSource.
type MockJobSource struct {
	name          string
	jobs          []*Job
	jobIndex      int
	acked         []*Job
	nacked        []*Job
	failed        []*Job
	failedData    []map[string]any
	connected     bool
	closed        bool
	mu            sync.Mutex
	cancelledJobs map[string]bool
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

func (m *MockJobSource) Fail(ctx context.Context, job *Job, err error, data map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed = append(m.failed, job)
	m.failedData = append(m.failedData, data)
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

func (m *MockJobSource) FailedJobs() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failed
}

func (m *MockJobSource) FailedData() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failedData
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
	claimed        bool
	claimedVersion string
	started        bool
	chunks         []string
	ended          bool
	errored        bool
	erroredErr     error
	erroredRecover bool
	cancelled      bool
}

func (m *MockStreamWriter) WriteClaimed(agentVersion string) error {
	m.claimed = true
	m.claimedVersion = agentVersion
	return nil
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
	m.erroredErr = err
	m.erroredRecover = recoverable
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

// TestRunnerUnsupportedJobTypeFailsTerminally verifies that a job whose type has
// no registered handler is terminally Failed (failed status + ACK) rather than
// Nacked. A Nack would leave the message pending in the consumer group and it
// would be redelivered by orphan recovery, re-failing forever, while the backend
// only ever saw an opaque dispatch timeout (issue #382).
func TestRunnerUnsupportedJobTypeFailsTerminally(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "UNKNOWN_JOB", Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("OTHER_JOB", false) // Doesn't handle UNKNOWN_JOB
	handlers := []JobHandler{handler}
	config := RunnerConfig{WorkerID: "test-worker", AgentVersion: "v2.46.0"}

	runner := NewRunner(source, handlers, config)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runner.Run(ctx)

	// The unsupported job must be terminally Failed (removed from the PEL),
	// NOT Nacked (which would leave it pending and retried forever).
	failed := source.FailedJobs()
	if len(failed) != 1 {
		t.Fatalf("Failed jobs = %d, want 1", len(failed))
	}
	if len(source.NackedJobs()) != 0 {
		t.Errorf("Nacked jobs = %d, want 0 (should Fail, not Nack)", len(source.NackedJobs()))
	}
	if len(source.AckedJobs()) != 0 {
		t.Errorf("Acked jobs = %d, want 0 (Fail carries its own ack, not a plain Ack)", len(source.AckedJobs()))
	}

	// The structured failure must carry the marker, the offending job type, and
	// the node's agent version -- this is what the backend surfaces as an
	// actionable "node vX.Y.Z doesn't support TYPE" message.
	data := source.FailedData()
	if len(data) != 1 {
		t.Fatalf("FailedData entries = %d, want 1", len(data))
	}
	d := data[0]
	if d["unsupported_job_type"] != true {
		t.Errorf("unsupported_job_type = %v, want true", d["unsupported_job_type"])
	}
	if d["job_type"] != "UNKNOWN_JOB" {
		t.Errorf("job_type = %v, want UNKNOWN_JOB", d["job_type"])
	}
	if d["agent_version"] != "v2.46.0" {
		t.Errorf("agent_version = %v, want v2.46.0", d["agent_version"])
	}

	// Handler should not have executed anything.
	if len(handler.ExecutedJobs()) != 0 {
		t.Errorf("Executed jobs = %d, want 0", len(handler.ExecutedJobs()))
	}
}

// TestRunnerUnsupportedJobTypePublishesTerminalError verifies that the
// unsupported-type path publishes a non-recoverable terminal error event
// through the stream writer. The streaming dispatch path waits on this event;
// without it the backend times out after ~30s (issue #382).
func TestRunnerUnsupportedJobTypePublishesTerminalError(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "COBROWSE", Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	handlers := []JobHandler{NewMockJobHandler("SHELL_COMMAND", false)}
	config := RunnerConfig{WorkerID: "test-worker", AgentVersion: "v2.46.0"}

	runner := NewRunner(source, handlers, config)

	stream := &MockStreamWriter{}
	runner.WithStreamWriterFactory(func(job *Job) StreamWriter { return stream })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runner.Run(ctx)

	if !stream.errored {
		t.Fatal("expected a terminal error event to be published for the unsupported type")
	}
	if stream.erroredRecover {
		t.Error("unsupported-type error should be non-recoverable")
	}
	if stream.erroredErr == nil {
		t.Fatal("expected a non-nil error on the published terminal event")
	}
	msg := stream.erroredErr.Error()
	if !strings.Contains(msg, "COBROWSE") || !strings.Contains(msg, "v2.46.0") {
		t.Errorf("error message = %q, want it to mention the job type and node version", msg)
	}
	if stream.ended {
		t.Error("WriteEnd should not be called for an unsupported job type")
	}
}

// TestRunnerKnownJobTypeStillDispatches guards against a regression: a job whose
// type IS registered must still dispatch to its handler and be Acked, never
// routed through the unsupported-type Fail path.
func TestRunnerKnownJobTypeStillDispatches(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "SHELL_COMMAND", Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("SHELL_COMMAND", false)
	handlers := []JobHandler{handler}
	config := RunnerConfig{WorkerID: "test-worker", AgentVersion: "v2.46.0"}

	runner := NewRunner(source, handlers, config)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runner.Run(ctx)

	if len(handler.ExecutedJobs()) != 1 {
		t.Errorf("Executed jobs = %d, want 1", len(handler.ExecutedJobs()))
	}
	if len(source.AckedJobs()) != 1 {
		t.Errorf("Acked jobs = %d, want 1", len(source.AckedJobs()))
	}
	if len(source.FailedJobs()) != 0 {
		t.Errorf("Failed jobs = %d, want 0 for a known type", len(source.FailedJobs()))
	}
}

// TestRunnerPublishesClaimedBeforeExecution verifies the claim-ack contract
// (aceteam#6000): when a job is read off the queue the runner publishes a
// "claimed" event carrying the node's agent version BEFORE any handler work, so
// the backend dispatcher can distinguish a live worker from a wedged/dead one
// within a short window.
func TestRunnerPublishesClaimedBeforeExecution(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "SHELL_COMMAND", Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("SHELL_COMMAND", false)
	config := RunnerConfig{WorkerID: "test-worker", AgentVersion: "v2.81.0"}

	runner := NewRunner(source, []JobHandler{handler}, config)

	stream := &MockStreamWriter{}
	runner.WithStreamWriterFactory(func(job *Job) StreamWriter { return stream })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runner.Run(ctx)

	if !stream.claimed {
		t.Fatal("expected a claimed event to be published when the job was read")
	}
	if stream.claimedVersion != "v2.81.0" {
		t.Errorf("claimed agent_version = %q, want v2.81.0", stream.claimedVersion)
	}
	if !stream.started {
		t.Error("expected the job to also proceed to WriteStart after claiming")
	}
}

// TestRunnerDoesNotClaimForeignTargetedJob verifies that a shared-stream message
// addressed to a different node (target_node mismatch) is skipped WITHOUT
// publishing a claim. Only the owning node claims; otherwise the dispatcher
// would see a false claim from a node that never runs the job.
func TestRunnerDoesNotClaimForeignTargetedJob(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "SHELL_COMMAND", Payload: map[string]any{"target_node": "other-node"}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("SHELL_COMMAND", false)
	config := RunnerConfig{WorkerID: "test-worker", NodeID: "this-node", AgentVersion: "v2.81.0"}

	runner := NewRunner(source, []JobHandler{handler}, config)

	stream := &MockStreamWriter{}
	runner.WithStreamWriterFactory(func(job *Job) StreamWriter { return stream })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runner.Run(ctx)

	if stream.claimed {
		t.Error("a job targeted at another node must not be claimed by this node")
	}
	if len(handler.ExecutedJobs()) != 0 {
		t.Errorf("Executed jobs = %d, want 0 for a foreign-targeted job", len(handler.ExecutedJobs()))
	}
}

// TestRunnerSupportedJobTypesReflectsRegistration verifies that the reported
// supported-types set only includes types the node actually has a handler for.
func TestRunnerSupportedJobTypesReflectsRegistration(t *testing.T) {
	source := NewMockJobSource("test", nil)
	handlers := []JobHandler{
		NewMockJobHandler(JobTypeShellCommand, false),
		NewMockJobHandler(JobTypeCobrowse, false),
	}
	runner := NewRunner(source, handlers, RunnerConfig{WorkerID: "test"})

	got := runner.supportedJobTypes()
	want := map[string]bool{JobTypeCobrowse: true, JobTypeShellCommand: true}
	if len(got) != len(want) {
		t.Fatalf("supportedJobTypes() = %v, want %d entries", got, len(want))
	}
	for _, jt := range got {
		if !want[jt] {
			t.Errorf("supportedJobTypes() included unexpected type %q", jt)
		}
	}
	// Result must be sorted for stable reporting.
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("supportedJobTypes() not sorted: %v", got)
		}
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

// --- target_node filter tests ---

// TestRunnerTargetNodeMismatchSkipsJob verifies that a job with a target_node
// that doesn't match this runner's NodeID is acknowledged and skipped without
// executing the handler or writing to the stream.
func TestRunnerTargetNodeMismatchSkipsJob(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "TEST_JOB", Payload: map[string]any{
			"target_node": "999",
			"command":     "hostname",
		}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("TEST_JOB", false)

	mockStream := &MockStreamWriter{}
	config := RunnerConfig{
		WorkerID: "test-worker",
		NodeID:   "1008", // This node's ID -- doesn't match target_node "999"
	}
	runner := NewRunner(source, []JobHandler{handler}, config)
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
	// Job should be acked (removed from this consumer's pending entries)
	if len(source.AckedJobs()) != 1 {
		t.Errorf("Expected 1 acked job, got %d", len(source.AckedJobs()))
	}
	// Stream should NOT have been written to (the correct node produces the result)
	if mockStream.started || mockStream.ended || mockStream.errored || mockStream.cancelled {
		t.Error("Stream writer should not be called for skipped jobs")
	}
}

// TestRunnerTargetNodeMatchProcessesJob verifies that a job with a target_node
// matching this runner's NodeID is processed normally.
func TestRunnerTargetNodeMatchProcessesJob(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "TEST_JOB", Payload: map[string]any{
			"target_node": "1008",
			"command":     "hostname",
		}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("TEST_JOB", false)

	config := RunnerConfig{
		WorkerID: "test-worker",
		NodeID:   "1008", // Matches target_node
	}
	runner := NewRunner(source, []JobHandler{handler}, config)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	// Handler should have executed
	if len(handler.ExecutedJobs()) != 1 {
		t.Errorf("Expected 1 executed job, got %d", len(handler.ExecutedJobs()))
	}
	// Job should be acked (successful)
	if len(source.AckedJobs()) != 1 {
		t.Errorf("Expected 1 acked job, got %d", len(source.AckedJobs()))
	}
}

// TestRunnerTargetNodeEmptyProcessesJob verifies that a job without a
// target_node field (broadcast job) is processed normally.
func TestRunnerTargetNodeEmptyProcessesJob(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "TEST_JOB", Payload: map[string]any{
			"command": "hostname",
		}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("TEST_JOB", false)

	config := RunnerConfig{
		WorkerID: "test-worker",
		NodeID:   "1008",
	}
	runner := NewRunner(source, []JobHandler{handler}, config)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	// Handler should have executed (no target_node means broadcast)
	if len(handler.ExecutedJobs()) != 1 {
		t.Errorf("Expected 1 executed job, got %d", len(handler.ExecutedJobs()))
	}
}

// TestRunnerTargetNodeEmptyStringProcessesJob verifies that a job with
// target_node set to an empty string is treated as a broadcast.
func TestRunnerTargetNodeEmptyStringProcessesJob(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "TEST_JOB", Payload: map[string]any{
			"target_node": "",
			"command":     "hostname",
		}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("TEST_JOB", false)

	config := RunnerConfig{
		WorkerID: "test-worker",
		NodeID:   "1008",
	}
	runner := NewRunner(source, []JobHandler{handler}, config)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	if len(handler.ExecutedJobs()) != 1 {
		t.Errorf("Expected 1 executed job, got %d", len(handler.ExecutedJobs()))
	}
}

// TestRunnerNodeIDEmptySkipsFilter verifies that when the runner's NodeID is
// empty (Headscale ID unresolved), the target_node filter is disabled and
// all jobs are processed -- including ones with a target_node set. This
// preserves pre-filter behavior and avoids dropping jobs that might be ours.
func TestRunnerNodeIDEmptySkipsFilter(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "TEST_JOB", Payload: map[string]any{
			"target_node": "999",
			"command":     "hostname",
		}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("TEST_JOB", false)

	config := RunnerConfig{
		WorkerID: "test-worker",
		NodeID:   "", // Empty -- Headscale ID not resolved
	}
	runner := NewRunner(source, []JobHandler{handler}, config)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	// Handler SHOULD have executed (filter disabled when NodeID is empty)
	if len(handler.ExecutedJobs()) != 1 {
		t.Errorf("Expected 1 executed job (filter should be disabled), got %d", len(handler.ExecutedJobs()))
	}
}

// TestRunnerTargetNodeMismatchNoUsageRecord verifies that skipped jobs do not
// produce usage records (which would attribute work to the wrong node).
func TestRunnerTargetNodeMismatchNoUsageRecord(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "TEST_JOB", Payload: map[string]any{
			"target_node": "999",
		}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler("TEST_JOB", false)

	var recordCount int
	config := RunnerConfig{
		WorkerID: "test-worker",
		NodeID:   "1008",
		JobRecordFn: func(record usage.UsageRecord) {
			recordCount++
		},
	}
	runner := NewRunner(source, []JobHandler{handler}, config)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	if recordCount != 0 {
		t.Errorf("Expected 0 usage records for skipped job, got %d", recordCount)
	}
}
