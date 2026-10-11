package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	jobhandlers "github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
)

// declaringHandler is the "fake capability-declaring handler" the #1287
// acceptance needs: it declares arbitrary JobCapabilities for one job type and
// returns a fixed result, proving the runner honors a DECLARATION with no edit to
// runner.go / deadline.go routing tables.
type declaringHandler struct {
	jobType string
	caps    jobhandlers.JobCapabilities
	result  *JobResult
	err     error
}

func (h *declaringHandler) CanHandle(jt string) bool { return jt == h.jobType }

func (h *declaringHandler) DeclaredCapabilities(jt string) (jobhandlers.JobCapabilities, bool) {
	if jt == h.jobType {
		return h.caps, true
	}
	return jobhandlers.JobCapabilities{}, false
}

func (h *declaringHandler) Execute(context.Context, *Job, StreamWriter) (*JobResult, error) {
	return h.result, h.err
}

// inferenceSuccess is a representative LLM_INFERENCE success output.
func inferenceSuccess() *JobResult {
	return &JobResult{Status: JobStatusSuccess, Output: map[string]any{
		"content": "the answer",
		"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
	}}
}

// TestRunnerSignsWorkReceiptForDeclaredRunnerSignedAdopter pins acceptance #2
// (citadel-cli#1287): a job whose handler DECLARED ReceiptOwnerRunner + a work
// action produces a signed work receipt AND a work record carrying the flow
// attribution fields — with zero edits to the runner's routing tables.
func TestRunnerSignsWorkReceiptForDeclaredRunnerSignedAdopter(t *testing.T) {
	handler := &declaringHandler{
		jobType: JobTypeLLMInference,
		caps:    jobhandlers.JobCapabilities{WorkActions: []string{jobhandlers.WorkActionInference}, ReceiptOwner: jobhandlers.ReceiptOwnerRunner},
		result:  inferenceSuccess(),
	}
	source := NewMockJobSource("test", nil)
	var recorded usage.UsageRecord
	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{
		WorkerID:          "test-worker",
		WorkReceiptSigner: newGoodSigner(t),
		WorkReceiptNodeID: "fabric-42",
		JobRecordFn:       func(r usage.UsageRecord) { recorded = r },
	})
	stream := &MockStreamWriter{}
	job := &Job{ID: "inf-1", Type: JobTypeLLMInference, Payload: map[string]any{
		"org_id":       "org_9",
		"flow_run_id":  "fr_1",
		"flow_node_id": "fn_2",
		"node_uid":     "uid_3",
		"receipt_ids":  []any{"rcpt_a"},
	}}

	if !runner.executeJob(context.Background(), job, stream, time.Now(), false, 0) {
		t.Fatal("a declared runner-signed inference success must report success")
	}
	if stream.endCount != 1 || stream.errorCount != 0 {
		t.Fatalf("expected exactly one terminal end: ends=%d errors=%d", stream.endCount, stream.errorCount)
	}
	// The signed work receipt is attached to the published end result.
	rcpts, ok := stream.endResult["work_receipts"].([]map[string]any)
	if !ok || len(rcpts) != 1 {
		t.Fatalf("expected one signed work receipt on the end result, got %T(%v)", stream.endResult["work_receipts"], stream.endResult["work_receipts"])
	}
	if rcpts[0]["action"] != "inference" || rcpts[0]["signature"] == "" || rcpts[0]["public_key_fingerprint"] == "" {
		t.Fatalf("work receipt not an inference signed receipt: %v", rcpts[0])
	}
	usg, ok := stream.endResult["usage"].(map[string]any)
	if !ok || usg["signed"] != true || usg["action"] != "inference" || usg["origin"] != "dispatched" || usg["org_id"] != "org_9" {
		t.Fatalf("usage attribution missing/incorrect: %v", stream.endResult["usage"])
	}
	// The work record carries the flow attribution fields from the envelope.
	if recorded.WorkAction != "inference" || recorded.Origin != "dispatched" || recorded.OrgID != "org_9" {
		t.Errorf("work record attribution = (%q,%q,%q)", recorded.WorkAction, recorded.Origin, recorded.OrgID)
	}
	if recorded.FlowRunID != "fr_1" || recorded.FlowNodeID != "fn_2" || recorded.NodeUID != "uid_3" {
		t.Errorf("work record flow attribution = (%q,%q,%q)", recorded.FlowRunID, recorded.FlowNodeID, recorded.NodeUID)
	}
	if len(recorded.ReceiptIDs) != 1 || recorded.ReceiptIDs[0] != "rcpt_a" {
		t.Errorf("work record receipt_ids = %v", recorded.ReceiptIDs)
	}
	if !recorded.ReceiptSigned {
		t.Error("work record should be marked receipt-signed")
	}
}

// TestRunnerSignedAdopterFailsClosedWhenSigningUnavailable pins the C7 fail-closed
// posture extended to a runner-signed adopter: a keyless node (signer resolves no
// fingerprint) refuses before executing — one terminal error, reason
// receipt_signing_unavailable, source.Fail (not Nack/retry).
func TestRunnerSignedAdopterFailsClosedWhenSigningUnavailable(t *testing.T) {
	handler := &declaringHandler{
		jobType: JobTypeLLMInference,
		caps:    jobhandlers.JobCapabilities{WorkActions: []string{jobhandlers.WorkActionInference}, ReceiptOwner: jobhandlers.ReceiptOwnerRunner},
		result:  inferenceSuccess(),
	}
	source := NewMockJobSource("test", nil)
	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{WorkerID: "test-worker", WorkReceiptSigner: fpErrSigner{}})
	stream := &MockStreamWriter{}
	job := &Job{ID: "inf-nokey", Type: JobTypeLLMInference, Payload: map[string]any{}}

	if runner.executeJob(context.Background(), job, stream, time.Now(), false, 0) {
		t.Fatal("a signing-unavailable runner-signed job must not report success")
	}
	if stream.errorCount != 1 || stream.endCount != 0 {
		t.Fatalf("expected exactly one terminal error: errors=%d ends=%d", stream.errorCount, stream.endCount)
	}
	if len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 {
		t.Fatalf("expected source.Fail only: failed=%d nacked=%d", len(source.FailedJobs()), len(source.NackedJobs()))
	}
	if source.FailedData()[0]["reason"] != "receipt_signing_unavailable" {
		t.Fatalf("failed-job metadata lost refusal reason: %#v", source.FailedData())
	}
}

// TestRunnerSignedAdopterNoSignerStaysUnsigned pins that a runner with NO signer
// wired (tests, legacy) never signs and never fails a runner-signed adopter
// closed — identical to the handler-signed "no signer => not dispatched" posture.
func TestRunnerSignedAdopterNoSignerStaysUnsigned(t *testing.T) {
	handler := &declaringHandler{
		jobType: JobTypeLLMInference,
		caps:    jobhandlers.JobCapabilities{WorkActions: []string{jobhandlers.WorkActionInference}, ReceiptOwner: jobhandlers.ReceiptOwnerRunner},
		result:  inferenceSuccess(),
	}
	source := NewMockJobSource("test", nil)
	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{WorkerID: "test-worker"}) // no signer
	stream := &MockStreamWriter{}
	if !runner.executeJob(context.Background(), &Job{ID: "inf-nosigner", Type: JobTypeLLMInference, Payload: map[string]any{}}, stream, time.Now(), false, 0) {
		t.Fatal("no-signer inference must still succeed")
	}
	if _, ok := stream.endResult["work_receipts"]; ok {
		t.Error("no signer wired but a runner-signed receipt was produced")
	}
}

// TestRunnerSkipsSigningForModelWarming pins that a model_warming success (no
// inference actually ran) is NOT signed — signing would attest work that did not
// happen (citadel-cli#1287).
func TestRunnerSkipsSigningForModelWarming(t *testing.T) {
	handler := &declaringHandler{
		jobType: JobTypeLLMInference,
		caps:    jobhandlers.JobCapabilities{WorkActions: []string{jobhandlers.WorkActionInference}, ReceiptOwner: jobhandlers.ReceiptOwnerRunner},
		result:  &JobResult{Status: JobStatusSuccess, Output: map[string]any{"status": "model_warming", "model": "m"}},
	}
	source := NewMockJobSource("test", nil)
	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{WorkerID: "test-worker", WorkReceiptSigner: newGoodSigner(t)})
	stream := &MockStreamWriter{}
	if !runner.executeJob(context.Background(), &Job{ID: "inf-warm", Type: JobTypeLLMInference, Payload: map[string]any{}}, stream, time.Now(), false, 0) {
		t.Fatal("model_warming is a clean success")
	}
	if _, ok := stream.endResult["work_receipts"]; ok {
		t.Error("model_warming must not be signed (no inference ran)")
	}
	if stream.endResult["status"] != "model_warming" {
		t.Errorf("end result lost model_warming status: %v", stream.endResult["status"])
	}
}

// TestRunnerNoActionHandlerNeverFailsClosedOnKeylessNode pins the CRITICAL
// fail-closed scope: a handler that registers NO work action (ReceiptOwnerNone)
// must NOT fail closed on a keyless node — e.g. SHELL_COMMAND still runs. The
// runner has a signer that cannot resolve a fingerprint, yet a no-action job
// succeeds untouched.
func TestRunnerNoActionHandlerNeverFailsClosedOnKeylessNode(t *testing.T) {
	handler := &declaringHandler{
		jobType: JobTypeShellCommand, // declares nothing → ReceiptOwnerNone
		caps:    jobhandlers.JobCapabilities{},
		result:  &JobResult{Status: JobStatusSuccess, Output: map[string]any{"output": "done"}},
	}
	source := NewMockJobSource("test", nil)
	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{WorkerID: "test-worker", WorkReceiptSigner: fpErrSigner{}})
	stream := &MockStreamWriter{}
	if !runner.executeJob(context.Background(), &Job{ID: "sh-1", Type: JobTypeShellCommand, Payload: map[string]any{}}, stream, time.Now(), false, 0) {
		t.Fatal("a no-work-action job must never fail closed on a keyless node")
	}
	if stream.errorCount != 0 || stream.endCount != 1 {
		t.Fatalf("expected a clean success: errors=%d ends=%d", stream.errorCount, stream.endCount)
	}
}

// TestRunnerHonorsFakeDeclaredHeavyLaneForNovelJobType pins acceptance #1
// (citadel-cli#1287): a handler for a BRAND-NEW job type that DECLARES
// Lane==LaneHeavy routes onto the heavy lane with NO edit to runner.go /
// deadline.go — proving adopters opt in purely by declaration.
func TestRunnerHonorsFakeDeclaredHeavyLaneForNovelJobType(t *testing.T) {
	const novelType = "CUSTOM_HEAVY_JOB"
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	heavy := &blockingJobHandler{
		jobType: novelType,
		onStart: func() { once.Do(func() { close(started) }) },
		release: release,
		caps:    &jobhandlers.JobCapabilities{Lane: jobhandlers.LaneHeavy, Deadline: jobhandlers.DeadlineLong},
	}
	other := NewMockJobHandler(JobTypeFileReadBytes, false)
	source := NewMockJobSource("test", []*Job{
		{ID: "heavy-1", Type: novelType, Payload: map[string]any{}},
		{ID: "read-1", Type: JobTypeFileReadBytes, Payload: map[string]any{}},
	})
	runner := NewRunner(source, []JobHandler{heavy, other}, RunnerConfig{WorkerID: "test", MaxConcurrency: 1, ActivityFn: func(string, string) {}})
	runner.WithStreamWriterFactory(func(*Job) StreamWriter { return &MockStreamWriter{} })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = runner.Run(ctx); close(runDone) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("novel heavy-declaring job never started")
	}
	// The novel heavy job is on the heavy lane (off the fetch loop), so the file
	// read completes while it is still executing.
	if !laneWaitFor(5*time.Second, func() bool { return len(other.ExecutedJobs()) >= 1 }) {
		t.Fatal("ordinary job did not run while the novel heavy job was in flight (declaration not honored)")
	}
	if !laneWaitFor(5*time.Second, func() bool {
		for _, s := range runner.LaneSnapshots() {
			if s.Lane == "heavy" && s.Executing >= 1 {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("heavy lane never showed the novel job executing; got %+v", runner.LaneSnapshots())
	}
	close(release)
	cancel()
	<-runDone
}
