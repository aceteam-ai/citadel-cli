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

	// requeueOnNack, when true, simulates RedisSource's real redelivery
	// behavior (issue #826): a Nack'd job whose delivery-attempt metadata says
	// another attempt is still within budget (willRetry) is re-appended to the
	// queue with Attempts incremented, so Next() returns it again on a later
	// call within the same Run(). Default false preserves every pre-existing
	// test's exact behavior (a Nack'd job is never seen again).
	requeueOnNack bool
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
	if m.requeueOnNack && willRetry(job) {
		redelivered := *job
		redelivered.Metadata.Attempts++
		m.jobs = append(m.jobs, &redelivered)
	}
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
//
// WriteEndErr / WriteErrorErr let a test simulate a failed publish (network
// blip, WS hiccup, transient API error) -- the exact scenario citadel-cli#559
// left unobservable when WriteEnd/WriteError's return value was discarded in
// Runner.processJob. The write is still recorded (ended/errored flip to true,
// as a real StreamWriter would have attempted the publish); only the RETURN
// VALUE is the injected error, so tests can assert both "the publish was
// attempted" and "the runner logged/handled the failure to publish."
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

	// endCount/errorCount count every WriteEnd/WriteError call, not just
	// whether one happened -- needed to assert "exactly one terminal event"
	// (issue #826) when the SAME writer instance is reused across multiple
	// processJob dispatches for one job id (a retry-then-succeed sequence).
	endCount   int
	errorCount int

	// WriteEndErr, when non-nil, is returned by WriteEnd instead of nil.
	WriteEndErr error
	// WriteErrorErr, when non-nil, is returned by WriteError instead of nil.
	WriteErrorErr error
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
	m.endCount++
	return m.WriteEndErr
}

func (m *MockStreamWriter) WriteError(err error, recoverable bool) error {
	m.errored = true
	m.erroredErr = err
	m.erroredRecover = recoverable
	m.errorCount++
	return m.WriteErrorErr
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

// TestRunnerNodeIDEmptyDeclinesAddressedJob is the regression test for
// citadel-cli#654. A node that could not resolve its Headscale ID must NOT
// execute a job addressed to some other node.
//
// The filter used to be gated on NodeID != "", so an unidentified node matched
// nothing and ran every addressed job it read off a shared stream -- executing
// on the wrong machine work the operator had aimed at a specific one. This
// asserts on the HANDLER (the job never runs), not merely on the ack, because
// acking a job we also executed would be the very bug.
func TestRunnerNodeIDEmptyDeclinesAddressedJob(t *testing.T) {
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

	if got := len(handler.ExecutedJobs()); got != 0 {
		t.Errorf("Expected 0 executed jobs (an unidentified node must not claim addressed work), got %d", got)
	}
	acked := source.AckedJobs()
	if len(acked) != 1 || acked[0].ID != "job-1" {
		t.Errorf("Expected the declined job to be acked so it leaves this node's pending list, got %d ack(s)", len(acked))
	}
}

// TestRunnerNodeIDEmptyStillRunsUntargetedJob pins the other half of #654: the
// fail-closed change must cost nothing for ORG-WIDE work. A job with no
// target_node is addressed to nobody in particular, so an unidentified node is
// still a legitimate server for it. Without this, "fail closed on identity"
// would silently become "an unidentified node serves nothing at all".
func TestRunnerNodeIDEmptyStillRunsUntargetedJob(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: "TEST_JOB", Payload: map[string]any{"command": "hostname"}},
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

	if got := len(handler.ExecutedJobs()); got != 1 {
		t.Errorf("Expected 1 executed job (untargeted work is still ours to serve), got %d", got)
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

// TestRunnerServiceStartPublishesTerminalEndOnSuccess pins the fix for
// citadel-cli#559: a SERVICE_START that completes successfully must publish
// exactly one terminal "end" event to stream:v1:{jobId} -- the backend
// subscribes BEFORE dispatch and waits on this event to short-circuit its
// slow poll-based fallback. Also guards against a double-publish (end AND
// error both firing for the same outcome).
func TestRunnerServiceStartPublishesTerminalEndOnSuccess(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: JobTypeServiceStart, Payload: map[string]any{"service": "vllm"}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler(JobTypeServiceStart, false) // succeeds
	config := RunnerConfig{WorkerID: "test-worker", AgentVersion: "v2.112.0"}

	runner := NewRunner(source, []JobHandler{handler}, config)

	stream := &MockStreamWriter{}
	runner.WithStreamWriterFactory(func(job *Job) StreamWriter { return stream })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	if !stream.ended {
		t.Error("expected a terminal 'end' event to be published for a successful SERVICE_START")
	}
	if stream.errored {
		t.Error("a successful SERVICE_START must not also publish a terminal error event (exactly one terminal event)")
	}
	if len(source.AckedJobs()) != 1 {
		t.Errorf("Acked jobs = %d, want 1", len(source.AckedJobs()))
	}
	if len(source.NackedJobs()) != 0 {
		t.Errorf("Nacked jobs = %d, want 0", len(source.NackedJobs()))
	}
}

// TestRunnerServiceStartPublishesTerminalErrorOnFailure is the failure-side
// counterpart of the above: a SERVICE_START whose handler fails must publish
// exactly one terminal "error" event, not silently Nack with nothing on the
// stream.
func TestRunnerServiceStartPublishesTerminalErrorOnFailure(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: JobTypeServiceStart, Payload: map[string]any{"service": "vllm"}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler(JobTypeServiceStart, true) // fails
	config := RunnerConfig{WorkerID: "test-worker", AgentVersion: "v2.112.0"}

	runner := NewRunner(source, []JobHandler{handler}, config)

	stream := &MockStreamWriter{}
	runner.WithStreamWriterFactory(func(job *Job) StreamWriter { return stream })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	if !stream.errored {
		t.Error("expected a terminal error event to be published for a failed SERVICE_START")
	}
	if stream.ended {
		t.Error("a failed SERVICE_START must not also publish a terminal end event (exactly one terminal event)")
	}
	if stream.erroredErr == nil {
		t.Fatal("expected a non-nil error on the published terminal event")
	}
	if len(source.NackedJobs()) != 1 {
		t.Errorf("Nacked jobs = %d, want 1", len(source.NackedJobs()))
	}
}

// TestRunnerServiceStartLogsWarningWhenTerminalEndPublishFails pins the actual
// behavioral change in citadel-cli#559: previously the success path discarded
// stream.WriteEnd's return value entirely, so a failed publish to
// stream:v1:{jobId} was invisible -- no log, no signal anywhere. This test
// forces WriteEnd to fail and asserts (a) the runner now logs a warning about
// it, and (b) the job still completes normally (Acked, not silently dropped)
// despite the publish failure -- the node's own bookkeeping must not regress
// just because the stream publish did.
func TestRunnerServiceStartLogsWarningWhenTerminalEndPublishFails(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: JobTypeServiceStart, Payload: map[string]any{"service": "vllm"}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler(JobTypeServiceStart, false) // succeeds

	var activityMu sync.Mutex
	var activityMessages []string
	config := RunnerConfig{
		WorkerID:     "test-worker",
		AgentVersion: "v2.112.0",
		ActivityFn: func(level, msg string) {
			activityMu.Lock()
			activityMessages = append(activityMessages, level+": "+msg)
			activityMu.Unlock()
		},
	}

	runner := NewRunner(source, []JobHandler{handler}, config)

	stream := &MockStreamWriter{WriteEndErr: errors.New("simulated publish failure (network blip)")}
	runner.WithStreamWriterFactory(func(job *Job) StreamWriter { return stream })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	if !stream.ended {
		t.Error("expected the runner to have attempted the terminal 'end' publish")
	}

	activityMu.Lock()
	messages := append([]string(nil), activityMessages...)
	activityMu.Unlock()

	foundWarning := false
	for _, m := range messages {
		if strings.Contains(m, "warning") && strings.Contains(m, "Failed to publish terminal end event") && strings.Contains(m, "job-1") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Errorf("expected a warning log about the failed terminal-end publish, got messages: %v", messages)
	}

	// Despite the publish failure, the job itself succeeded on the node and
	// must still be Acked -- the publish failure must not turn into a job
	// failure or a silently dropped job.
	if len(source.AckedJobs()) != 1 {
		t.Errorf("Acked jobs = %d, want 1 (publish failure must not block Ack of a locally-successful job)", len(source.AckedJobs()))
	}
	if len(source.NackedJobs()) != 0 {
		t.Errorf("Nacked jobs = %d, want 0", len(source.NackedJobs()))
	}
}

// TestRunnerServiceStartLogsWarningWhenTerminalErrorPublishFails is the
// failure-side counterpart: previously stream.WriteError's return value was
// discarded on the generic handler-failure path too. Forces WriteError to
// fail and asserts the runner logs a warning and still Nacks the job.
func TestRunnerServiceStartLogsWarningWhenTerminalErrorPublishFails(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: JobTypeServiceStart, Payload: map[string]any{"service": "vllm"}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler(JobTypeServiceStart, true) // fails

	var activityMu sync.Mutex
	var activityMessages []string
	config := RunnerConfig{
		WorkerID:     "test-worker",
		AgentVersion: "v2.112.0",
		ActivityFn: func(level, msg string) {
			activityMu.Lock()
			activityMessages = append(activityMessages, level+": "+msg)
			activityMu.Unlock()
		},
	}

	runner := NewRunner(source, []JobHandler{handler}, config)

	stream := &MockStreamWriter{WriteErrorErr: errors.New("simulated publish failure (network blip)")}
	runner.WithStreamWriterFactory(func(job *Job) StreamWriter { return stream })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	if !stream.errored {
		t.Error("expected the runner to have attempted the terminal error publish")
	}

	activityMu.Lock()
	messages := append([]string(nil), activityMessages...)
	activityMu.Unlock()

	foundWarning := false
	for _, m := range messages {
		if strings.Contains(m, "warning") && strings.Contains(m, "Failed to publish terminal error event") && strings.Contains(m, "job-1") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Errorf("expected a warning log about the failed terminal-error publish, got messages: %v", messages)
	}

	// The job itself failed on the node and must still be Nacked, regardless
	// of whether the stream publish also failed.
	if len(source.NackedJobs()) != 1 {
		t.Errorf("Nacked jobs = %d, want 1 (publish failure must not block Nack of a locally-failed job)", len(source.NackedJobs()))
	}
	if len(source.AckedJobs()) != 0 {
		t.Errorf("Acked jobs = %d, want 0", len(source.AckedJobs()))
	}
}

// TestRunnerNoGPUSlotsNacksWithoutHandlerExecution pins the CURRENT (deliberately
// unchanged) behavior of the GPU-slot-exhaustion path FOR A GENUINELY GPU-BOUND
// JOB TYPE: the job is Nacked for redelivery without running the handler. This
// Nack redelivers the same job ID, so a caller streaming this attempt would need
// to already treat a Nack as non-terminal -- unlike a genuine terminal outcome
// (success/failure), there is no terminal-event contract for a will-be-retried
// attempt. See the PR for citadel-cli#559 for why a terminal-event publish was
// deliberately NOT added here (it risks turning a transparent retry into a
// reported failure if the backend treats any published error as final).
//
// Uses JobTypeLLMInference (a real GPU-bound type per gpuBoundJobTypes) rather
// than an arbitrary "TEST_JOB" -- citadel-cli#825 scoped the GPU-slot gate to
// GPU-bound job types only, so a made-up type would no longer exercise this
// path at all. See TestRunnerNonGPUJobSkipsGPUSlotGate for the non-GPU case.
func TestRunnerNoGPUSlotsNacksWithoutHandlerExecution(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: JobTypeLLMInference, Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler(JobTypeLLMInference, false)
	config := RunnerConfig{
		WorkerID:   "test-worker",
		GPUTracker: NewGPUTracker(0), // zero slots: every Acquire() fails
	}

	runner := NewRunner(source, []JobHandler{handler}, config)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	if len(handler.ExecutedJobs()) != 0 {
		t.Errorf("handler should not run when no GPU slot is available, got %d executions", len(handler.ExecutedJobs()))
	}
	if len(source.NackedJobs()) != 1 {
		t.Errorf("Nacked jobs = %d, want 1", len(source.NackedJobs()))
	}
}

// TestRunnerNonGPUJobSkipsGPUSlotGate is the regression test for citadel-cli#825.
// It models the ACTUAL production scenario the issue describes: 1 GPU, but
// MaxConcurrency raised above the GPU count (2), and the sole GPU slot already
// held by an in-flight inference job (simulated by pre-acquiring it, rather than
// racing a real concurrent job -- this pins the "more in-flight than GPU slots"
// state directly instead of depending on goroutine scheduling). Under exactly
// that contention, a non-GPU-bound job like SERVICE_START must NOT be routed
// through the GPU-slot acquire/Nack path at all. Before the fix, this Nacked
// with zero published terminal events -- a SERVICE_START would vanish from the
// backend's stream:v1:{jobId} waiter with no success, failure, or even a retry
// signal it could distinguish from a dead node. See
// TestRunnerGPUJobStillNacksUnderRealContention for proof the genuine GPU path
// is unchanged under the identical setup.
func TestRunnerNonGPUJobSkipsGPUSlotGate(t *testing.T) {
	tracker := NewGPUTracker(1) // 1 GPU
	if _, ok := tracker.Acquire(); !ok {
		t.Fatal("precondition: expected to acquire the tracker's only slot")
	} // now held by a simulated in-flight inference job, mirroring MaxConcurrency > GPU count

	jobs := []*Job{
		{ID: "job-1", Type: JobTypeServiceStart, Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler(JobTypeServiceStart, false)
	config := RunnerConfig{
		WorkerID:       "test-worker",
		MaxConcurrency: 2, // > GPU count (1) -- the exact operator config #825 describes
		GPUTracker:     tracker,
	}

	runner := NewRunner(source, []JobHandler{handler}, config)

	stream := &MockStreamWriter{}
	runner.WithStreamWriterFactory(func(job *Job) StreamWriter { return stream })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	if len(handler.ExecutedJobs()) != 1 {
		t.Errorf("handler should run despite the GPU slot being held (non-GPU job type), got %d executions", len(handler.ExecutedJobs()))
	}
	if len(source.NackedJobs()) != 0 {
		t.Errorf("Nacked jobs = %d, want 0 -- a non-GPU job must never be Nacked by GPU-slot contention", len(source.NackedJobs()))
	}
	if len(source.AckedJobs()) != 1 {
		t.Errorf("Acked jobs = %d, want 1", len(source.AckedJobs()))
	}
	if !stream.ended {
		t.Error("expected a terminal WriteEnd for the successful non-GPU job")
	}
}

// TestRunnerGPUJobStillNacksUnderRealContention proves #825's fix did not
// regress the genuine GPU-bound path: under the identical contention setup as
// TestRunnerNonGPUJobSkipsGPUSlotGate (1 GPU slot, already held, MaxConcurrency
// raised above the GPU count), a real inference job (JobTypeLLMInference) still
// hits the GPU-slot gate and is Nacked without running the handler -- exactly
// the pre-#825 behavior, just now scoped to job types that actually need it.
func TestRunnerGPUJobStillNacksUnderRealContention(t *testing.T) {
	tracker := NewGPUTracker(1)
	if _, ok := tracker.Acquire(); !ok {
		t.Fatal("precondition: expected to acquire the tracker's only slot")
	}

	jobs := []*Job{
		{ID: "job-1", Type: JobTypeLLMInference, Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler(JobTypeLLMInference, false)
	config := RunnerConfig{
		WorkerID:       "test-worker",
		MaxConcurrency: 2,
		GPUTracker:     tracker,
	}

	runner := NewRunner(source, []JobHandler{handler}, config)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	if len(handler.ExecutedJobs()) != 0 {
		t.Errorf("handler should not run when the sole GPU slot is already held, got %d executions", len(handler.ExecutedJobs()))
	}
	if len(source.NackedJobs()) != 1 {
		t.Errorf("Nacked jobs = %d, want 1", len(source.NackedJobs()))
	}
}

// flakyThenSucceedsHandler fails on the first delivered attempt
// (job.Metadata.Attempts <= 1) and succeeds on any later attempt. Combined
// with MockJobSource's requeueOnNack, this lets a test drive a real
// transient-fail-then-retry-succeeds sequence through Runner.processJob
// twice for the SAME job id, the exact shape of issue #826's bug.
type flakyThenSucceedsHandler struct {
	jobType   string
	mu        sync.Mutex
	execCount int
}

func (h *flakyThenSucceedsHandler) CanHandle(jobType string) bool { return h.jobType == jobType }

func (h *flakyThenSucceedsHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	h.mu.Lock()
	h.execCount++
	h.mu.Unlock()

	if job.Metadata.Attempts <= 1 {
		err := errors.New("transient failure")
		return &JobResult{Status: JobStatusFailure, Error: err}, err
	}
	return &JobResult{Status: JobStatusSuccess, Output: map[string]any{"ok": true}}, nil
}

func (h *flakyThenSucceedsHandler) ExecCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.execCount
}

// TestRunnerTransientFailureThenSuccessPublishesExactlyOneEndEvent is issue
// #826's core regression test. Before the fix, processJob published a
// terminal "error" unconditionally before every Nack -- including a Nack that
// gets redelivered and later succeeds -- so a job that failed transiently and
// then succeeded on retry published BOTH an "error" and an "end" on the same
// stream:v1:{jobId}, violating "exactly one terminal event per job id".
//
// job-1's first delivery carries Metadata{Attempts:1, MaxAttempts:3} (so
// willRetry is true: another attempt is still within budget) and the handler
// fails. MockJobSource's requeueOnNack redelivers it as Attempts:2, on which
// the handler succeeds. The SAME MockStreamWriter instance is used for both
// dispatches (job id is stable), so endCount/errorCount are cumulative across
// the whole retry sequence.
func TestRunnerTransientFailureThenSuccessPublishesExactlyOneEndEvent(t *testing.T) {
	jobType := "FLAKY_JOB"
	jobs := []*Job{
		{
			ID:      "job-1",
			Type:    jobType,
			Payload: map[string]any{},
			Metadata: JobMetadata{
				Attempts:    1,
				MaxAttempts: 3,
			},
		},
	}

	source := NewMockJobSource("test", jobs)
	source.requeueOnNack = true
	handler := &flakyThenSucceedsHandler{jobType: jobType}
	config := RunnerConfig{WorkerID: "test-worker"}

	runner := NewRunner(source, []JobHandler{handler}, config)

	stream := &MockStreamWriter{}
	runner.WithStreamWriterFactory(func(job *Job) StreamWriter { return stream })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	if handler.ExecCount() != 2 {
		t.Fatalf("handler executions = %d, want 2 (fail once, then succeed on retry)", handler.ExecCount())
	}
	if stream.endCount != 1 {
		t.Errorf("endCount = %d, want 1", stream.endCount)
	}
	if stream.errorCount != 0 {
		t.Errorf("errorCount = %d, want 0 (the transient failure must not publish a terminal error)", stream.errorCount)
	}
	if len(source.NackedJobs()) != 1 {
		t.Errorf("Nacked jobs = %d, want 1 (the transient failure)", len(source.NackedJobs()))
	}
	if len(source.AckedJobs()) != 1 {
		t.Errorf("Acked jobs = %d, want 1 (the eventual success)", len(source.AckedJobs()))
	}
}

// TestRunnerExhaustedRetriesPublishesExactlyOneErrorEvent is the failure-side
// counterpart: a job on its FINAL allowed attempt (Attempts+1 == MaxAttempts,
// so willRetry is false -- no further redelivery will ever reach a handler)
// must still publish exactly one terminal error, so a job that exhausts its
// retries reports failure once rather than silently (see willRetry's doc
// comment on why MaxAttempts>0 is what makes this attempt "final" rather than
// "unknown").
func TestRunnerExhaustedRetriesPublishesExactlyOneErrorEvent(t *testing.T) {
	jobType := "ALWAYS_FAILS_JOB"
	jobs := []*Job{
		{
			ID:      "job-1",
			Type:    jobType,
			Payload: map[string]any{},
			Metadata: JobMetadata{
				Attempts:    2, // final dispatched attempt: 2+1 == MaxAttempts
				MaxAttempts: 3,
			},
		},
	}

	source := NewMockJobSource("test", jobs)
	// requeueOnNack left false: even if set, willRetry(job) is false here, so
	// no redelivery would happen regardless -- this is the exhausted case.
	handler := NewMockJobHandler(jobType, true) // always fails
	config := RunnerConfig{WorkerID: "test-worker"}

	runner := NewRunner(source, []JobHandler{handler}, config)

	stream := &MockStreamWriter{}
	runner.WithStreamWriterFactory(func(job *Job) StreamWriter { return stream })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	if stream.errorCount != 1 {
		t.Errorf("errorCount = %d, want 1 (the exhausted final attempt must still report failure)", stream.errorCount)
	}
	if stream.endCount != 0 {
		t.Errorf("endCount = %d, want 0", stream.endCount)
	}
	if len(source.NackedJobs()) != 1 {
		t.Errorf("Nacked jobs = %d, want 1", len(source.NackedJobs()))
	}
}

// TestRunnerFailureWithNoRetrySignalPublishesTerminalError pins the
// conservative fallback in willRetry: a job with no populated
// Attempts/MaxAttempts metadata (MaxAttempts == 0, e.g. an APISource job --
// the AceTeam Redis API proxy exposes no per-message delivery count -- or any
// job predating this signal) is treated as "unknown, assume terminal" and
// still publishes on a generic failure, exactly matching pre-#826 behavior.
// This is also what TestRunnerServiceStartPublishesTerminalErrorOnFailure
// already relies on; this test names the reason explicitly.
func TestRunnerFailureWithNoRetrySignalPublishesTerminalError(t *testing.T) {
	jobs := []*Job{
		{ID: "job-1", Type: JobTypeServiceStart, Payload: map[string]any{"service": "vllm"}},
	}

	source := NewMockJobSource("test", jobs)
	handler := NewMockJobHandler(JobTypeServiceStart, true) // fails
	config := RunnerConfig{WorkerID: "test-worker"}

	runner := NewRunner(source, []JobHandler{handler}, config)

	stream := &MockStreamWriter{}
	runner.WithStreamWriterFactory(func(job *Job) StreamWriter { return stream })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	runner.Run(ctx)

	if stream.errorCount != 1 {
		t.Errorf("errorCount = %d, want 1 (no retry signal must default to publishing, not suppressing)", stream.errorCount)
	}
}

// blockingJobHandler blocks in Execute until release is closed or ctx is
// cancelled, calling onStart the first time Execute begins. Used by
// TestRunnerLongSessionJobDoesNotBlockOtherJobs (citadel-cli#489) to hold a
// MEETING_JOIN job in flight while asserting a later job still completes.
type blockingJobHandler struct {
	jobType string
	onStart func()
	release chan struct{}

	mu       sync.Mutex
	executed []*Job
}

func (h *blockingJobHandler) CanHandle(jobType string) bool { return h.jobType == jobType }

func (h *blockingJobHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	h.mu.Lock()
	h.executed = append(h.executed, job)
	h.mu.Unlock()

	if h.onStart != nil {
		h.onStart()
	}

	select {
	case <-h.release:
		return &JobResult{Status: JobStatusSuccess, Output: map[string]any{"ok": true}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *blockingJobHandler) ExecutedJobs() []*Job {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.executed
}

// keyedStreamWriterFactory hands out one MockStreamWriter per job ID, so a
// test with multiple in-flight jobs can inspect each job's terminal events
// independently.
type keyedStreamWriterFactory struct {
	mu      sync.Mutex
	writers map[string]*MockStreamWriter
}

func newKeyedStreamWriterFactory() *keyedStreamWriterFactory {
	return &keyedStreamWriterFactory{writers: make(map[string]*MockStreamWriter)}
}

func (f *keyedStreamWriterFactory) factory(job *Job) StreamWriter {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.writers[job.ID]
	if !ok {
		w = &MockStreamWriter{}
		f.writers[job.ID] = w
	}
	return w
}

func (f *keyedStreamWriterFactory) get(jobID string) *MockStreamWriter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writers[jobID]
}

func jobIDs(jobs []*Job) []string {
	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	return ids
}

// TestRunnerLongSessionJobDoesNotBlockOtherJobs is the regression test for
// citadel-cli#489: on a maxConcurrency=1 node (the GPU-less default that
// cmd/work.go picks), a long-session job type (MEETING_JOIN, per
// longSessionJobTypes in deadline.go) must not occupy the node's only
// sequential slot for the length of a multi-hour meeting and starve every
// other poll cycle. It should now dispatch on its own always-async goroutine,
// independent of maxConcurrency, so a job queued right behind it is fetched
// and completed while the meeting is still in flight -- and it must still run
// through the same per-job machinery (watchdog/deadline, terminal-event
// publishing, WorkerState in-flight accounting, ack) as every other job.
func TestRunnerLongSessionJobDoesNotBlockOtherJobs(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once

	meetingHandler := &blockingJobHandler{
		jobType: JobTypeMeetingJoin,
		onStart: func() { startOnce.Do(func() { close(started) }) },
		release: release,
	}
	shellHandler := NewMockJobHandler(JobTypeShellCommand, false)

	jobs := []*Job{
		{ID: "meeting-1", Type: JobTypeMeetingJoin, Payload: map[string]any{}},
		{ID: "shell-1", Type: JobTypeShellCommand, Payload: map[string]any{}},
	}
	source := NewMockJobSource("test", jobs)
	state := NewWorkerState()
	config := RunnerConfig{
		WorkerID:       "test-worker",
		MaxConcurrency: 1, // the GPU-less default that triggers #489's head-of-line blocking
		State:          state,
	}
	runner := NewRunner(source, []JobHandler{meetingHandler, shellHandler}, config)

	streams := newKeyedStreamWriterFactory()
	runner.WithStreamWriterFactory(streams.factory)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(runDone)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("MEETING_JOIN handler never started")
	}

	// While the meeting job is still blocked in its handler, the shell job
	// must still be fetched and dispatched -- the core #489 assertion. Before
	// the fix, MEETING_JOIN ran inline on the sole sequential slot and
	// SHELL_COMMAND was never even fetched until the meeting finished.
	deadline := time.Now().Add(2 * time.Second)
	var shellDispatched bool
	for time.Now().Before(deadline) {
		if len(shellHandler.ExecutedJobs()) >= 1 {
			shellDispatched = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !shellDispatched {
		t.Fatal("SHELL_COMMAND was not dispatched while MEETING_JOIN was still in flight (head-of-line blocking regression)")
	}

	// Give the shell job's terminal ack a moment to land.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(source.AckedJobs()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	acked := source.AckedJobs()
	if len(acked) != 1 || acked[0].ID != "shell-1" {
		t.Fatalf("expected exactly shell-1 to be acked while the meeting job is blocked, got %v", jobIDs(acked))
	}

	shellStream := streams.get("shell-1")
	if shellStream == nil || !shellStream.ended {
		t.Error("expected a terminal WriteEnd for the shell job")
	}

	meetingStream := streams.get("meeting-1")
	if meetingStream == nil || !meetingStream.started {
		t.Error("expected the meeting job's handler to have published WriteStart")
	}
	if meetingStream != nil && meetingStream.ended {
		t.Error("meeting job should not have a terminal event yet -- its handler is still blocked")
	}

	// In-flight accounting (WorkerState, #548 self-heal / liveness) must still
	// reflect the blocked meeting job even though it's on the async lane.
	if snap := state.Snapshot(); snap.InFlight < 1 {
		t.Errorf("WorkerState.InFlight = %d, want >= 1 while MEETING_JOIN is still running", snap.InFlight)
	}

	// Release the meeting handler so it can finish through the normal success
	// path, and wait for it to actually be acked -- deliberately BEFORE
	// cancelling the context below, so the handler's select always picks the
	// (already-closed) release channel rather than racing it against ctx.Done().
	close(release)

	deadline = time.Now().Add(2 * time.Second)
	var meetingAcked bool
	for time.Now().Before(deadline) {
		for _, j := range source.AckedJobs() {
			if j.ID == "meeting-1" {
				meetingAcked = true
			}
		}
		if meetingAcked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !meetingAcked {
		t.Fatal("meeting-1 was not acked after releasing its handler")
	}

	if len(meetingHandler.ExecutedJobs()) != 1 {
		t.Fatalf("meeting handler executions = %d, want 1", len(meetingHandler.ExecutedJobs()))
	}
	if meetingStream := streams.get("meeting-1"); meetingStream == nil || !meetingStream.ended {
		t.Error("expected a terminal WriteEnd for the meeting job after it completed")
	}

	if snap := state.Snapshot(); snap.InFlight != 0 {
		t.Errorf("WorkerState.InFlight = %d, want 0 after both jobs finished", snap.InFlight)
	}

	// Both jobs are done; cancel rather than wait out the full 5s timeout so
	// the test doesn't need to sit through it just to observe clean shutdown.
	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}
}
