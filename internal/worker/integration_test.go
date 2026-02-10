package worker

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

// recordingStreamWriter captures all StreamWriter calls for verification.
type recordingStreamWriter struct {
	mu              sync.Mutex
	startMessage    string
	endResult       map[string]any
	cancelledReason string
	errMessage      string
	started         bool
	ended           bool
	cancelled       bool
	errored         bool
}

func (w *recordingStreamWriter) WriteStart(message string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.started = true
	w.startMessage = message
	return nil
}

func (w *recordingStreamWriter) WriteChunk(content string, index int) error {
	return nil
}

func (w *recordingStreamWriter) WriteEnd(result map[string]any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ended = true
	w.endResult = result
	return nil
}

func (w *recordingStreamWriter) WriteError(err error, recoverable bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.errored = true
	if err != nil {
		w.errMessage = err.Error()
	}
	return nil
}

func (w *recordingStreamWriter) WriteCancelled(reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancelled = true
	w.cancelledReason = reason
	return nil
}

// recordingStreamWriterFactory creates recordingStreamWriters and captures the
// rayID that was on the job at factory invocation time.
type recordingStreamWriterFactory struct {
	mu      sync.Mutex
	writers map[string]*recordingStreamWriter // jobID -> writer
	rayIDs  map[string]string                 // jobID -> rayID
}

func newRecordingStreamWriterFactory() *recordingStreamWriterFactory {
	return &recordingStreamWriterFactory{
		writers: make(map[string]*recordingStreamWriter),
		rayIDs:  make(map[string]string),
	}
}

func (f *recordingStreamWriterFactory) factory(job *Job) StreamWriter {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &recordingStreamWriter{}
	f.writers[job.ID] = w
	f.rayIDs[job.ID] = job.RayID
	return w
}

func (f *recordingStreamWriterFactory) getWriter(jobID string) *recordingStreamWriter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writers[jobID]
}

func (f *recordingStreamWriterFactory) getRayID(jobID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rayIDs[jobID]
}

// setupWorkerIntegration starts miniredis and creates a connected RedisSource.
func setupWorkerIntegration(t *testing.T, queueName string, maxAttempts int) (*miniredis.Miniredis, *RedisSource, *goredis.Client) {
	t.Helper()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	t.Cleanup(func() { mr.Close() })

	source := NewRedisSource(RedisSourceConfig{
		URL:           "redis://" + mr.Addr(),
		QueueName:     queueName,
		ConsumerGroup: "test-workers",
		BlockMs:       100,
		MaxAttempts:   maxAttempts,
		LogFn:         func(level, msg string) {}, // suppress output
	})

	ctx := context.Background()
	if err := source.Connect(ctx); err != nil {
		t.Fatalf("source.Connect failed: %v", err)
	}
	t.Cleanup(func() { source.Close() })

	raw := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { raw.Close() })

	return mr, source, raw
}

// enqueueTestJob adds a job to a Redis Stream with the given fields.
func enqueueTestJob(t *testing.T, raw *goredis.Client, queue string, fields map[string]interface{}) string {
	t.Helper()
	ctx := context.Background()
	msgID, err := raw.XAdd(ctx, &goredis.XAddArgs{
		Stream: queue,
		Values: fields,
	}).Result()
	if err != nil {
		t.Fatalf("failed to enqueue job: %v", err)
	}
	return msgID
}

// TestRunnerRayIDPropagation verifies end-to-end rayId flow:
// Redis Stream field → redis.Job.RawData → worker.Job.RayID → StreamWriter factory
func TestRunnerRayIDPropagation(t *testing.T) {
	queue := "jobs:v1:ray-integration"
	_, source, raw := setupWorkerIntegration(t, queue, 3)
	ctx := context.Background()

	jobID := "job-ray-integration-001"
	rayID := "ray-integration-001"

	// Enqueue job with rayId as a top-level stream field
	payload, _ := json.Marshal(map[string]interface{}{"prompt": "hello"})
	enqueueTestJob(t, raw, queue, map[string]interface{}{
		"jobId":   jobID,
		"type":    "TEST_JOB",
		"payload": string(payload),
		"rayId":   rayID,
	})

	handler := NewMockJobHandler("TEST_JOB", false)
	factory := newRecordingStreamWriterFactory()

	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{
		WorkerID:   "test-worker",
		ActivityFn: func(level, msg string) {},
	})
	runner.WithStreamWriterFactory(factory.factory)

	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	runner.Run(runCtx)

	// Handler should have executed the job
	executed := handler.ExecutedJobs()
	if len(executed) != 1 {
		t.Fatalf("expected 1 executed job, got %d", len(executed))
	}

	// Verify rayID was propagated from Redis Stream through to the StreamWriter factory
	gotRayID := factory.getRayID(jobID)
	if gotRayID != rayID {
		t.Errorf("factory received rayId = %q, want %q", gotRayID, rayID)
	}

	// Verify the stream writer received start and end calls
	w := factory.getWriter(jobID)
	if w == nil {
		t.Fatal("no stream writer created for job")
	}
	if !w.started {
		t.Error("expected WriteStart to be called")
	}
	if !w.ended {
		t.Error("expected WriteEnd to be called")
	}

	// Verify the job itself had the correct rayID
	if executed[0].RayID != rayID {
		t.Errorf("executed job rayId = %q, want %q", executed[0].RayID, rayID)
	}
}

// TestRunnerCancellationIntegration verifies end-to-end cancellation:
// Set cancellation key in Redis → worker checks IsJobCancelled → handler NOT called → WriteCancelled → ACK
func TestRunnerCancellationIntegration(t *testing.T) {
	queue := "jobs:v1:cancel-integration"
	mr, source, raw := setupWorkerIntegration(t, queue, 3)
	ctx := context.Background()

	jobID := "job-cancel-integration-002"
	rayID := "ray-cancel-002"

	// Enqueue the job
	payload, _ := json.Marshal(map[string]interface{}{"data": "test"})
	enqueueTestJob(t, raw, queue, map[string]interface{}{
		"jobId":   jobID,
		"type":    "TEST_JOB",
		"payload": string(payload),
		"rayId":   rayID,
	})

	// Set cancellation flag BEFORE worker claims it (JQS-Core Section 5.6)
	mr.Set("job:cancelled:"+jobID, "1")

	handler := NewMockJobHandler("TEST_JOB", false)
	factory := newRecordingStreamWriterFactory()

	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{
		WorkerID:   "test-worker",
		ActivityFn: func(level, msg string) {},
	})
	runner.WithStreamWriterFactory(factory.factory)

	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	runner.Run(runCtx)

	// Handler should NOT have executed (job was cancelled)
	if len(handler.ExecutedJobs()) != 0 {
		t.Errorf("expected 0 executed jobs (cancelled), got %d", len(handler.ExecutedJobs()))
	}

	// WriteCancelled should have been called
	w := factory.getWriter(jobID)
	if w == nil {
		t.Fatal("no stream writer created for cancelled job")
	}
	if !w.cancelled {
		t.Error("expected WriteCancelled to be called")
	}

	// Verify the rayID was propagated even for cancelled jobs
	gotRayID := factory.getRayID(jobID)
	if gotRayID != rayID {
		t.Errorf("cancelled job factory received rayId = %q, want %q", gotRayID, rayID)
	}

	// Job should be ACK'd (no pending entries in the stream)
	pending, err := raw.XPending(ctx, queue, "test-workers").Result()
	if err != nil {
		t.Fatalf("XPending failed: %v", err)
	}
	if pending.Count != 0 {
		t.Errorf("expected 0 pending entries (job should be ACK'd), got %d", pending.Count)
	}
}

// TestRunnerDLQEnqueuedAt verifies end-to-end DLQ flow:
// Enqueue job with enqueuedAt → handler fails → DLQ entry preserves enqueuedAt
func TestRunnerDLQEnqueuedAt(t *testing.T) {
	queue := "jobs:v1:dlq-integration"
	_, source, raw := setupWorkerIntegration(t, queue, 1) // MaxAttempts=1 for immediate DLQ
	ctx := context.Background()

	jobID := "job-dlq-integration-003"
	enqueuedAt := "2025-01-15T12:00:00Z"

	// Enqueue job with enqueuedAt field
	payload, _ := json.Marshal(map[string]interface{}{"data": "will-fail"})
	enqueueTestJob(t, raw, queue, map[string]interface{}{
		"jobId":      jobID,
		"type":       "FAIL_JOB",
		"payload":    string(payload),
		"enqueuedAt": enqueuedAt,
	})

	// Create a handler that always fails
	handler := NewMockJobHandler("FAIL_JOB", true)

	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{
		WorkerID:   "test-worker",
		ActivityFn: func(level, msg string) {},
	})

	// With MaxAttempts=1:
	// 1st read: handler fails, Nack (no ACK — left pending)
	// 2nd read: delivery count >= MaxAttempts, RedisSource moves to DLQ and ACKs
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	runner.Run(runCtx)

	// Verify DLQ entry exists with preserved enqueuedAt
	dlqName := "dlq:v1:dlq-integration"
	msgs, err := raw.XRange(ctx, dlqName, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange on DLQ failed: %v", err)
	}

	if len(msgs) == 0 {
		t.Fatal("expected at least 1 DLQ entry")
	}

	// Find our job's DLQ entry
	var found bool
	for _, msg := range msgs {
		if msg.Values["jobId"] == jobID {
			found = true
			if msg.Values["enqueuedAt"] != enqueuedAt {
				t.Errorf("DLQ enqueuedAt = %q, want %q", msg.Values["enqueuedAt"], enqueuedAt)
			}
			break
		}
	}
	if !found {
		t.Errorf("DLQ entry for %s not found", jobID)
	}
}
