package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	jobhandlers "github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// ctxHonoringHeavyHandler is a FILE_INDEX worker.JobHandler that honors the
// deadline context and records when its Execute actually returns, so a test can
// prove executeWithDeadline waited for it (no orphaned writer).
type ctxHonoringHeavyHandler struct {
	returned chan struct{}
}

func (h *ctxHonoringHeavyHandler) CanHandle(jt string) bool { return jt == JobTypeFileIndex }

func (h *ctxHonoringHeavyHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	<-ctx.Done() // honor the deadline promptly
	// Simulate a brief in-flight upsert finishing before the goroutine exits.
	time.Sleep(10 * time.Millisecond)
	close(h.returned)
	return nil, ctx.Err()
}

// TestExecuteWithDeadlineHeavyDrainsBeforeReturning pins acceptance #2
// (aceteam#10876 C1): when a heavy-lane job's (FILE_INDEX) deadline fires,
// executeWithDeadline waits for the handler goroutine to actually finish (and
// therefore close its index DB) before returning, so no writer goroutine is left
// running against the index when the next job can be admitted.
func TestExecuteWithDeadlineHeavyDrainsBeforeReturning(t *testing.T) {
	handler := &ctxHonoringHeavyHandler{returned: make(chan struct{})}
	r := NewRunner(NewMockJobSource("test", nil), []JobHandler{handler}, RunnerConfig{
		WorkerID:   "test-worker",
		ActivityFn: func(string, string) {},
	})

	_, err := r.executeWithDeadline(context.Background(), handler,
		&Job{ID: "idx-1", Type: JobTypeFileIndex}, &MockStreamWriter{}, 50*time.Millisecond)

	var de *deadlineExceededError
	if !errors.As(err, &de) {
		t.Fatalf("error = %v, want *deadlineExceededError", err)
	}
	// The handler goroutine must already be done — executeWithDeadline drained it.
	select {
	case <-handler.returned:
	default:
		t.Fatal("executeWithDeadline returned before the heavy-lane handler goroutine finished (writer not drained)")
	}
}

// adapterProbeHandler is a jobs.JobHandler that records what the adapter wired
// into its JobContext.
type adapterProbeHandler struct {
	sawCancel bool
}

func (h *adapterProbeHandler) Execute(ctx jobhandlers.JobContext, job *nexus.Job) ([]byte, error) {
	h.sawCancel = ctx.CancelRequestedNow()
	ctx.EmitProgress(jobhandlers.ProgressEvent{
		Stage: "index", Done: 1, Total: 2, Unit: "files", Current: "x.md", Final: true,
	})
	return []byte(`{"ok":true}`), nil
}

// TestLegacyAdapterWiresProgressAndCancel pins that the LegacyHandlerAdapter
// threads the cooperative-cancel predicate from the context into
// JobContext.CancelRequested and forwards JobContext.Progress to the stream
// (aceteam#10876 C1), so a legacy handler like FILE_INDEX can observe both.
func TestLegacyAdapterWiresProgressAndCancel(t *testing.T) {
	probe := &adapterProbeHandler{}
	adapter := NewLegacyHandlerAdapter("PROBE", probe)
	stream := &MockStreamWriter{}
	ctx := jobhandlers.WithCancelRequested(context.Background(), func() bool { return true })

	res, err := adapter.Execute(ctx, &Job{ID: "p", Type: "PROBE"}, stream)
	if err != nil {
		t.Fatalf("adapter.Execute: %v", err)
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %v, want success", res.Status)
	}
	if !probe.sawCancel {
		t.Error("handler did not observe the cooperative-cancel predicate wired via the context")
	}
	if len(stream.progress) == 0 {
		t.Fatal("progress event was not forwarded to the stream writer")
	}
	last := stream.progress[len(stream.progress)-1]
	if last["stage"] != "index" || last["current"] != "x.md" {
		t.Errorf("forwarded progress = %v, want stage=index current=x.md", last)
	}
}
