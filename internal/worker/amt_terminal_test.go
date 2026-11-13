package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

type failingAMTHandler struct {
	err   error
	write bool
}

func (failingAMTHandler) CanHandle(kind string) bool { return kind == JobTypeAMTKVM }

func (h failingAMTHandler) Execute(ctx context.Context, _ *Job, _ StreamWriter) (*JobResult, error) {
	if h.write {
		trace := jobs.AMTMutationTraceFrom(ctx)
		if trace.BeginWrite() {
			trace.EndWrite(true)
		}
	}
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
		{"unobserved-marker", "power", jobs.ErrAMTOutcomeUnknown, "amt_failed_no_automatic_retry", false},
		{"typed-passcode", "keys", jobs.AMTRefusal(jobs.ReasonPasscodeInvalid), "amt_failed_no_automatic_retry", false},
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
			runner := NewRunner(source, []JobHandler{failingAMTHandler{err: tc.err, write: tc.unknown}}, RunnerConfig{WorkerID: "test-worker"})
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
			var refusal *jobs.ShellRefusal
			if errors.As(tc.err, &refusal) && stream.erroredErr != tc.err {
				t.Fatal("typed prewrite refusal changed before publication")
			}
			if stream.errorCount != 1 || stream.erroredErr == nil || strings.Contains(stream.erroredErr.Error(), "outcome unknown") != tc.unknown {
				t.Fatalf("terminal error = %v, want unknown=%t", stream.erroredErr, tc.unknown)
			}
		})
	}
}

type stalledAMTHandler struct {
	writeInFlight bool
	entered       chan struct{}
	release       chan struct{}
	done          chan struct{}
}

func (*stalledAMTHandler) CanHandle(kind string) bool { return kind == JobTypeAMTKVM }

func (h *stalledAMTHandler) Execute(ctx context.Context, _ *Job, _ StreamWriter) (*JobResult, error) {
	defer close(h.done)
	trace := jobs.AMTMutationTraceFrom(ctx)
	if h.writeInFlight {
		trace.BeginWrite()
	}
	close(h.entered)
	<-h.release
	if h.writeInFlight {
		trace.EndWrite(false)
	}
	err := errors.New("synthetic handler stopped")
	return &JobResult{Status: JobStatusFailure, Error: err}, err
}

func TestAMTWatchdogDistinguishesPrewriteAndInflight(t *testing.T) {
	for _, writeInFlight := range []bool{false, true} {
		name := "prewrite"
		wantReason := "amt_failed_no_automatic_retry"
		if writeInFlight {
			name = "inflight-write"
			wantReason = "amt_outcome_unknown"
		}
		t.Run(name, func(t *testing.T) {
			handler := &stalledAMTHandler{
				writeInFlight: writeInFlight,
				entered:       make(chan struct{}), release: make(chan struct{}), done: make(chan struct{}),
			}
			source := NewMockJobSource("test", nil)
			runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{WorkerID: "test-worker"})
			stream := &MockStreamWriter{}
			job := &Job{
				ID: "amt-watchdog", Type: JobTypeAMTKVM,
				Payload:  map[string]any{"operation": "keys", "timeout_ms": int64(100)},
				Metadata: JobMetadata{Attempts: 1, MaxAttempts: 3},
			}
			completed := make(chan bool, 1)
			go func() {
				completed <- runner.executeJob(context.Background(), job, stream, time.Now(), false, 0)
			}()
			select {
			case <-handler.entered:
			case <-time.After(2 * time.Second):
				close(handler.release)
				t.Fatal("AMT handler did not enter before watchdog")
			}
			var succeeded bool
			select {
			case succeeded = <-completed:
			case <-time.After(2 * time.Second):
				close(handler.release)
				t.Fatal("AMT watchdog did not return")
			}
			if succeeded {
				t.Fatal("abandoned AMT handler reported success")
			}
			close(handler.release)
			<-handler.done
			if len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 {
				t.Fatalf("watchdog must be terminal: failed=%d nacked=%d", len(source.FailedJobs()), len(source.NackedJobs()))
			}
			if got := source.FailedData()[0]["reason"]; got != wantReason {
				t.Fatalf("reason=%v, want %s", got, wantReason)
			}
			if stream.erroredErr == nil || strings.Contains(stream.erroredErr.Error(), "outcome unknown") != writeInFlight {
				t.Fatalf("published error=%v, write in flight=%t", stream.erroredErr, writeInFlight)
			}
		})
	}
}

type lateFirstAMTHandler struct {
	entered, release, done chan struct{}
	admitted               chan bool
}

func (*lateFirstAMTHandler) CanHandle(kind string) bool { return kind == JobTypeAMTKVM }
func (h *lateFirstAMTHandler) Execute(ctx context.Context, _ *Job, _ StreamWriter) (*JobResult, error) {
	defer close(h.done)
	close(h.entered)
	<-h.release
	trace := jobs.AMTMutationTraceFrom(ctx)
	admitted := trace.BeginWrite()
	h.admitted <- admitted
	if admitted {
		trace.EndWrite(true)
	}
	err := errors.New("synthetic late handler completion")
	return &JobResult{Status: JobStatusFailure, Error: err}, err
}
func TestAMTWatchdogBlocksLateFirstWriteAndKeepsTerminalReason(t *testing.T) {
	handler := &lateFirstAMTHandler{entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{}), admitted: make(chan bool, 1)}
	source := NewMockJobSource("test", nil)
	stream := &MockStreamWriter{}
	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{WorkerID: "fixture"})
	job := &Job{ID: "late-amt", Type: JobTypeAMTKVM, Payload: map[string]any{"operation": "power", "timeout_ms": int64(100)}, Metadata: JobMetadata{Attempts: 1, MaxAttempts: 3}}
	completed := make(chan bool, 1)
	go func() { completed <- runner.executeJob(context.Background(), job, stream, time.Now(), false, 0) }()
	select {
	case <-handler.entered:
	case <-time.After(2 * time.Second):
		close(handler.release)
		t.Fatal("handler did not enter")
	}
	select {
	case success := <-completed:
		if success {
			t.Fatal("watchdog returned success")
		}
	case <-time.After(2 * time.Second):
		close(handler.release)
		t.Fatal("watchdog did not finish")
	}
	close(handler.release)
	<-handler.done
	if <-handler.admitted {
		t.Fatal("abandoned handler admitted a late first mutating write")
	}
	if len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 || source.FailedData()[0]["reason"] != "amt_failed_no_automatic_retry" {
		t.Fatal("late handler changed the terminal classification or redelivered")
	}
	if stream.errorCount != 1 || strings.Contains(stream.erroredErr.Error(), "outcome unknown") {
		t.Fatal("late completion replaced the prewrite terminal error")
	}
}
