package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
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

type releaseAfterCancelHeavyHandler struct {
	cancelObserved chan struct{}
	release        chan struct{}
}

func (h *releaseAfterCancelHeavyHandler) CanHandle(jt string) bool { return jt == JobTypeFileIndex }

func (h *releaseAfterCancelHeavyHandler) Execute(ctx context.Context, _ *Job, _ StreamWriter) (*JobResult, error) {
	<-ctx.Done()
	close(h.cancelObserved)
	<-h.release
	return &JobResult{Status: JobStatusSuccess}, nil
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

func TestExecuteWithDeadlineHeavyWaitsForActualExitWithoutEscape(t *testing.T) {
	handler := &releaseAfterCancelHeavyHandler{cancelObserved: make(chan struct{}), release: make(chan struct{})}
	r := NewRunner(NewMockJobSource("test", nil), []JobHandler{handler}, RunnerConfig{WorkerID: "test-worker", ActivityFn: func(string, string) {}})
	done := make(chan error, 1)
	go func() {
		_, err := r.executeWithDeadline(context.Background(), handler,
			&Job{ID: "idx-held", Type: JobTypeFileIndex}, &MockStreamWriter{}, 20*time.Millisecond)
		done <- err
	}()
	select {
	case <-handler.cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("heavy handler did not observe deadline cancellation")
	}
	select {
	case err := <-done:
		t.Fatalf("executeWithDeadline returned before actual heavy exit: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(handler.release)
	select {
	case err := <-done:
		var de *deadlineExceededError
		if !errors.As(err, &de) {
			t.Fatalf("error=%v, want deadlineExceededError", err)
		}
	case <-time.After(time.Second):
		t.Fatal("executeWithDeadline did not return after actual handler exit")
	}
}

func TestExecuteWithDeadlineHeavyShutdownWaitsForActualExit(t *testing.T) {
	handler := &releaseAfterCancelHeavyHandler{cancelObserved: make(chan struct{}), release: make(chan struct{})}
	r := NewRunner(NewMockJobSource("test", nil), []JobHandler{handler}, RunnerConfig{WorkerID: "test-worker", ActivityFn: func(string, string) {}})
	parent, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.executeWithDeadline(parent, handler,
			&Job{ID: "idx-shutdown", Type: JobTypeFileIndex}, &MockStreamWriter{}, time.Hour)
		done <- err
	}()
	cancel()
	select {
	case <-handler.cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("heavy handler did not observe shutdown")
	}
	select {
	case err := <-done:
		t.Fatalf("shutdown returned before heavy handler exit: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(handler.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return after heavy handler exited")
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
		Stage: jobhandlers.ProgressStageEmbed, Done: 1, Total: 2, Unit: "files", Current: "x.md", Final: true,
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
	if last["stage"] != "embed" || last["current"] != "x.md" {
		t.Errorf("forwarded progress = %v, want stage=embed current=x.md", last)
	}
}

type completionOrder struct {
	mu     sync.Mutex
	events []string
}

func (o *completionOrder) add(event string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
}

func (o *completionOrder) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

type orderedAckSource struct {
	*MockJobSource
	order *completionOrder
	acked chan struct{}
	once  sync.Once
}

func (s *orderedAckSource) Ack(ctx context.Context, job *Job) error {
	s.order.add("ack")
	err := s.MockJobSource.Ack(ctx, job)
	s.once.Do(func() { close(s.acked) })
	return err
}

type orderedEndWriter struct {
	MockStreamWriter
	order *completionOrder
}

func (w *orderedEndWriter) WriteEnd(result map[string]any) error {
	w.order.add("end")
	return w.MockStreamWriter.WriteEnd(result)
}

func TestRunnerLegacyFileIndexPublishesNestedResultBeforeAck(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"max_client_batch_size":32}`)) })
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode embedding request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(req.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float64{1, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": req.Model})
	})
	tei := httptest.NewServer(mux)
	defer tei.Close()
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_HNSW", "false")

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "document.md"), []byte("local document content"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	legacy := jobhandlers.NewFileIndexHandler(ws, dbPath)
	adapter := NewLegacyHandlerAdapter(JobTypeFileIndex, legacy)
	job := &Job{ID: "actual-file-index", Type: JobTypeFileIndex, Payload: map[string]any{"path": ws}}
	order := &completionOrder{}
	source := &orderedAckSource{
		MockJobSource: NewMockJobSource("test", []*Job{job}),
		order:         order,
		acked:         make(chan struct{}),
	}
	writer := &orderedEndWriter{order: order}
	runner := NewRunner(source, []JobHandler{adapter}, RunnerConfig{WorkerID: "test", MaxConcurrency: 1, ActivityFn: func(string, string) {}})
	runner.WithStreamWriterFactory(func(*Job) StreamWriter { return writer })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	runDone := make(chan struct{})
	go func() { _ = runner.Run(ctx); close(runDone) }()
	select {
	case <-source.acked:
	case <-ctx.Done():
		cancel()
		t.Fatalf("actual FILE_INDEX was not ACKed: %v", ctx.Err())
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("runner did not stop after actual FILE_INDEX completion")
	}

	if got := order.snapshot(); len(got) != 2 || got[0] != "end" || got[1] != "ack" {
		t.Fatalf("terminal ordering=%v, want exactly [end ack]", got)
	}
	if writer.endCount != 1 {
		t.Fatalf("terminal end count=%d, want exactly one", writer.endCount)
	}
	nested, ok := writer.endResult["output"].(string)
	if !ok {
		t.Fatalf("terminal result.output=%T(%v), want nested JSON string", writer.endResult["output"], writer.endResult["output"])
	}
	var result struct {
		FilesIndexed int    `json:"files_indexed"`
		Status       string `json:"status"`
		Checkpoint   string `json:"checkpoint"`
	}
	if err := json.Unmarshal([]byte(nested), &result); err != nil {
		t.Fatalf("decode nested FILE_INDEX output: %v; body=%q", err, nested)
	}
	if result.FilesIndexed != 1 || result.Status != "completed" || result.Checkpoint != "complete" {
		t.Fatalf("nested FILE_INDEX result=%+v, want one indexed and completed/complete", result)
	}
}
