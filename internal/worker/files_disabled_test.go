package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunnerFilesDisabledPublishesTerminalErrorWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata JobMetadata
	}{
		{"first_redis_delivery", JobMetadata{Attempts: 1, MaxAttempts: 3}},
		{"final_redis_delivery", JobMetadata{Attempts: 2, MaxAttempts: 3}},
		{"without_delivery_metadata", JobMetadata{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			source := NewMockJobSource("test", nil)
			handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
				WorkspaceDir: workspace, FilesDisabled: true,
			})
			runner := NewRunner(source, handlers, RunnerConfig{WorkerID: "test-worker"})
			stream := &MockStreamWriter{}
			job := &Job{
				ID: "refused-upload", Type: JobTypeFileWriteBytes,
				Payload:  map[string]any{"path": "denied.bin", "content": "YWJj"},
				Metadata: tc.metadata,
			}

			if runner.executeJob(context.Background(), job, stream, time.Now(), false, 0) {
				t.Fatal("a permission refusal must not report job success")
			}
			if stream.errorCount != 1 || stream.endCount != 0 || stream.erroredRecover {
				t.Fatalf("expected exactly one non-recoverable error: errors=%d ends=%d recoverable=%v", stream.errorCount, stream.endCount, stream.erroredRecover)
			}
			var refusal struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(stream.erroredErr.Error()), &refusal); err != nil || refusal.Reason != "files_disabled" {
				t.Fatalf("terminal event lost typed permission refusal: reason=%q err=%v", refusal.Reason, err)
			}
			if len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 || len(source.AckedJobs()) != 0 {
				t.Fatalf("expected source.Fail (failed status plus ACK), never retry or success ACK: failed=%d nacked=%d acked=%d", len(source.FailedJobs()), len(source.NackedJobs()), len(source.AckedJobs()))
			}
			if source.FailedData()[0]["reason"] != "files_disabled" {
				t.Fatalf("failed-job metadata lost refusal reason: %#v", source.FailedData())
			}
			if _, err := os.Stat(filepath.Join(workspace, "denied.bin")); !os.IsNotExist(err) {
				t.Fatalf("refused write touched workspace: %v", err)
			}
		})
	}
}
