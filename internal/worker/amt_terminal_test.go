package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type failingAMTHandler struct{}

func (failingAMTHandler) CanHandle(kind string) bool { return kind == JobTypeAMTKVM }

func (failingAMTHandler) Execute(context.Context, *Job, StreamWriter) (*JobResult, error) {
	err := errors.New("AMT response timed out")
	return &JobResult{Status: JobStatusFailure, Error: err}, err
}

func TestAMTFailureNeverRedeliversMutation(t *testing.T) {
	for _, tc := range []struct {
		operation  string
		wantReason string
		unknown    bool
	}{
		{"power", "amt_outcome_unknown", true},
		{"keys", "amt_outcome_unknown", true},
		{"type", "amt_outcome_unknown", true},
		{"status", "amt_failed_no_automatic_retry", false},
		{"screenshot", "amt_failed_no_automatic_retry", false},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			source := NewMockJobSource("test", nil)
			runner := NewRunner(source, []JobHandler{failingAMTHandler{}}, RunnerConfig{WorkerID: "test-worker"})
			stream := &MockStreamWriter{}
			job := &Job{
				ID: "amt-fixture", Type: JobTypeAMTKVM,
				Payload:  map[string]any{"operation": tc.operation},
				Metadata: JobMetadata{Attempts: 1, MaxAttempts: 3},
			}
			if runner.executeJob(context.Background(), job, stream, time.Now(), false, 0) {
				t.Fatal("failed AMT job reported success")
			}
			if len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 {
				t.Fatalf("AMT failure must be terminal: failed=%d nacked=%d", len(source.FailedJobs()), len(source.NackedJobs()))
			}
			if source.FailedData()[0]["reason"] != tc.wantReason {
				t.Fatalf("reason = %v, want %s", source.FailedData()[0]["reason"], tc.wantReason)
			}
			if stream.errorCount != 1 || stream.erroredErr == nil || strings.Contains(stream.erroredErr.Error(), "outcome unknown") != tc.unknown {
				t.Fatalf("terminal error = %v, want unknown=%t", stream.erroredErr, tc.unknown)
			}
		})
	}
}
