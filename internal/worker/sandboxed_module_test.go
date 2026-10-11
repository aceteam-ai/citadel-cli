package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

// sandboxUnavailableHandler returns the typed sandboxed-module refusal, standing
// in for a future #1264 extractor handler that cannot run the sandbox on this
// node.
type sandboxUnavailableHandler struct{ jobType string }

func (h *sandboxUnavailableHandler) CanHandle(jobType string) bool { return h.jobType == jobType }
func (h *sandboxUnavailableHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	return nil, jobs.ErrSandboxedModuleUnavailable
}

// TestRunnerSandboxedModuleUnavailablePublishesTerminalErrorWithoutRetry pins the
// runner's terminal-branch wiring for citadel-cli#1288: a job refused with
// jobs.ErrSandboxedModuleUnavailable fails CLOSED -- exactly one terminal error
// event, source.Fail with reason "sandboxed_module_unavailable", never a Nack --
// even on a first delivery with retry budget remaining. A sandboxed module runs
// with no network and cannot pull its image, so a retry cannot fix it; this is
// the same posture as ErrFilesDisabled / ErrReceiptSigningUnavailable.
func TestRunnerSandboxedModuleUnavailablePublishesTerminalErrorWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata JobMetadata
	}{
		{"first_redis_delivery", JobMetadata{Attempts: 1, MaxAttempts: 3}},
		{"final_redis_delivery", JobMetadata{Attempts: 2, MaxAttempts: 3}},
		{"without_delivery_metadata", JobMetadata{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := NewMockJobSource("test", nil)
			runner := NewRunner(source, []JobHandler{&sandboxUnavailableHandler{jobType: "SANDBOXED_HELPER"}}, RunnerConfig{WorkerID: "test-worker"})
			stream := &MockStreamWriter{}
			job := &Job{ID: "needs-sandbox", Type: "SANDBOXED_HELPER", Metadata: tc.metadata}

			if runner.executeJob(context.Background(), job, stream, time.Now(), false, 0) {
				t.Fatal("a sandboxed-module-unavailable refusal must not report job success")
			}
			if stream.errorCount != 1 || stream.endCount != 0 || stream.erroredRecover {
				t.Fatalf("expected exactly one non-recoverable terminal error: errors=%d ends=%d recoverable=%v", stream.errorCount, stream.endCount, stream.erroredRecover)
			}
			var refusal struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(stream.erroredErr.Error()), &refusal); err != nil || refusal.Reason != "sandboxed_module_unavailable" {
				t.Fatalf("terminal event lost typed refusal: reason=%q err=%v", refusal.Reason, err)
			}
			if len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 || len(source.AckedJobs()) != 0 {
				t.Fatalf("expected source.Fail (failed status plus ACK), never retry or success ACK: failed=%d nacked=%d acked=%d", len(source.FailedJobs()), len(source.NackedJobs()), len(source.AckedJobs()))
			}
			if source.FailedData()[0]["reason"] != "sandboxed_module_unavailable" {
				t.Fatalf("failed-job metadata lost refusal reason: %#v", source.FailedData())
			}
		})
	}
}
