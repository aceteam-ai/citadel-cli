package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

type failingAMTHandler struct{ err error }

func (failingAMTHandler) CanHandle(kind string) bool { return kind == JobTypeAMTKVM }

func (h failingAMTHandler) Execute(context.Context, *Job, StreamWriter) (*JobResult, error) {
	return &JobResult{Status: JobStatusFailure, Error: h.err}, h.err
}

func TestAMTFailureNeverRedeliversMutation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		operation  string
		err        error
		wantReason string
		unknown    bool
	}{
		{"power-refused", "power", errors.New("AMT confirmation refused"), "amt_failed_no_automatic_retry", false},
		{"keys-refused", "keys", errors.New("AMT permission refused"), "amt_failed_no_automatic_retry", false},
		{"type-refused", "type", errors.New("AMT credential refused"), "amt_failed_no_automatic_retry", false},
		{"power-written", "power", jobs.ErrAMTOutcomeUnknown, "amt_outcome_unknown", true},
		{"keys-written", "keys", jobs.ErrAMTOutcomeUnknown, "amt_outcome_unknown", true},
		{"type-written", "type", jobs.ErrAMTOutcomeUnknown, "amt_outcome_unknown", true},
		{"status", "status", errors.New("AMT status failed"), "amt_failed_no_automatic_retry", false},
		{"screenshot", "screenshot", errors.New("AMT screenshot failed"), "amt_failed_no_automatic_retry", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := NewMockJobSource("test", nil)
			runner := NewRunner(source, []JobHandler{failingAMTHandler{err: tc.err}}, RunnerConfig{WorkerID: "test-worker"})
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
