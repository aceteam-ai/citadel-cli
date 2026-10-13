package worker

import (
	"context"

	redisclient "github.com/aceteam-ai/citadel-cli/internal/redis"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// RedisStreamWriter implements StreamWriter using Redis Pub/Sub.
// This enables real-time streaming of job output to subscribers.
type RedisStreamWriter struct {
	clock  *deliveryClock
	client *redisclient.Client
	jobID  string
	rayID  string
	ctx    context.Context
}

// NewRedisStreamWriter creates a new Redis-backed stream writer.
func NewRedisStreamWriter(ctx context.Context, client *redisclient.Client, jobID, rayID string) *RedisStreamWriter {
	return &RedisStreamWriter{
		client: client,
		jobID:  jobID,
		rayID:  rayID,
		ctx:    ctx,
	}
}

// WriteClaimed signals that this worker has claimed the job (read it off the
// queue, before handler execution). See StreamWriter.WriteClaimed.
func (w *RedisStreamWriter) WriteClaimed(agentVersion string) error {
	return w.client.PublishStreamEvent(w.ctx, w.jobID, w.rayID, "claimed", w.clock.data(map[string]any{"agent_version": agentVersion}, false))
}

// WriteStart signals the beginning of job processing.
func (w *RedisStreamWriter) WriteStart(message string) error {
	w.client.SetJobStatus(w.ctx, w.jobID, "processing", nil)
	return w.client.PublishStart(w.ctx, w.jobID, w.rayID, message)
}

// WriteChunk sends an incremental output chunk (e.g., LLM token).
func (w *RedisStreamWriter) WriteChunk(content string, index int) error {
	return w.client.PublishChunk(w.ctx, w.jobID, w.rayID, content, index)
}

// WriteEnd signals successful job completion with final result.
func (w *RedisStreamWriter) WriteEnd(result map[string]any) error {
	return w.client.PublishStreamEvent(w.ctx, w.jobID, w.rayID, "end", w.clock.data(map[string]any{"result": result}, true))
}

// WriteError signals job failure.
func (w *RedisStreamWriter) WriteError(err error, recoverable bool) error {
	return w.client.PublishStreamEvent(w.ctx, w.jobID, w.rayID, "error", w.clock.data(map[string]any{"error": err.Error(), "recoverable": recoverable}, true))
}

// WriteCancelled signals job cancellation.
func (w *RedisStreamWriter) WriteCancelled(reason string) error {
	w.client.SetJobStatus(w.ctx, w.jobID, "cancelled", nil)
	return w.client.PublishStreamEvent(w.ctx, w.jobID, w.rayID, "cancelled", w.clock.data(map[string]any{"reason": reason}, true))
}

// WriteProgress publishes a throttled progress event on the job's pub/sub stream
// and mirrors the latest snapshot into the bounded job:<id>:progress hash. The
// hash mirror is best-effort (a dropped mirror never fails the job); the pub/sub
// event is the primary path.
func (w *RedisStreamWriter) WriteProgress(event map[string]any) error {
	_ = w.client.SetJobProgress(w.ctx, w.jobID, event)
	return w.client.PublishStreamEvent(w.ctx, w.jobID, w.rayID, "progress", event)
}

// Ensure RedisStreamWriter implements StreamWriter
var _ StreamWriter = (*RedisStreamWriter)(nil)

// CreateRedisStreamWriterFactory returns a factory function for creating Redis stream writers.
// This is used with Runner.WithStreamWriterFactory().
func CreateRedisStreamWriterFactory(ctx context.Context, source *RedisSource) func(job *Job) StreamWriter {
	return func(job *Job) StreamWriter {
		return NewRedisStreamWriter(ctx, source.Client(), job.ID, job.RayID)
	}
}

// APIStreamWriter implements StreamWriter using the Redis API proxy.
// This enables real-time streaming of job output via the secure API.
type APIStreamWriter struct {
	clock  *deliveryClock
	client *redisapi.Client
	jobID  string
	rayID  string
	ctx    context.Context
}

// NewAPIStreamWriter creates a new API-backed stream writer.
func NewAPIStreamWriter(ctx context.Context, client *redisapi.Client, jobID, rayID string) *APIStreamWriter {
	return &APIStreamWriter{
		client: client,
		jobID:  jobID,
		rayID:  rayID,
		ctx:    ctx,
	}
}

// WriteClaimed signals that this worker has claimed the job (read it off the
// queue, before handler execution). See StreamWriter.WriteClaimed.
func (w *APIStreamWriter) WriteClaimed(agentVersion string) error {
	return w.client.PublishStreamEvent(w.ctx, w.jobID, w.rayID, "claimed", w.clock.data(map[string]any{"agent_version": agentVersion}, false))
}

// WriteStart signals the beginning of job processing.
func (w *APIStreamWriter) WriteStart(message string) error {
	w.client.SetJobStatus(w.ctx, w.jobID, "processing", nil)
	return w.client.PublishStart(w.ctx, w.jobID, w.rayID, message)
}

// WriteChunk sends an incremental output chunk (e.g., LLM token).
func (w *APIStreamWriter) WriteChunk(content string, index int) error {
	return w.client.PublishChunk(w.ctx, w.jobID, w.rayID, content, index)
}

// WriteEnd signals successful job completion with final result.
func (w *APIStreamWriter) WriteEnd(result map[string]any) error {
	return w.client.PublishStreamEvent(w.ctx, w.jobID, w.rayID, "end", w.clock.data(map[string]any{"result": result}, true))
}

// WriteError signals job failure.
func (w *APIStreamWriter) WriteError(err error, recoverable bool) error {
	return w.client.PublishStreamEvent(w.ctx, w.jobID, w.rayID, "error", w.clock.data(map[string]any{"error": err.Error(), "recoverable": recoverable}, true))
}

// WriteCancelled signals job cancellation.
func (w *APIStreamWriter) WriteCancelled(reason string) error {
	w.client.SetJobStatus(w.ctx, w.jobID, "cancelled", nil)
	return w.client.PublishStreamEvent(w.ctx, w.jobID, w.rayID, "cancelled", w.clock.data(map[string]any{"reason": reason}, true))
}

// WriteProgress publishes a throttled progress event via the API proxy and
// mirrors the latest snapshot into the bounded job:<id>:progress key. The mirror
// is best-effort; the pub/sub event is the primary path.
func (w *APIStreamWriter) WriteProgress(event map[string]any) error {
	_ = w.client.SetJobProgress(w.ctx, w.jobID, event)
	return w.client.PublishStreamEvent(w.ctx, w.jobID, w.rayID, "progress", event)
}

// Ensure APIStreamWriter implements StreamWriter
var _ StreamWriter = (*APIStreamWriter)(nil)

// CreateAPIStreamWriterFactory returns a factory function for creating API stream writers.
// This is used with Runner.WithStreamWriterFactory().
func CreateAPIStreamWriterFactory(ctx context.Context, source *APISource) func(job *Job) StreamWriter {
	return func(job *Job) StreamWriter {
		return NewAPIStreamWriter(ctx, source.Client(), job.ID, job.RayID)
	}
}

// clockStreamWriter observes existing carriers without creating any event or
// changing the handler/client interface. Test/custom writers still see exactly
// their original calls; real writers add metadata beside unchanged result data.
type clockStreamWriter struct {
	StreamWriter
	clock *deliveryClock
}

func observeStream(writer StreamWriter, clock *deliveryClock) StreamWriter {
	if clock == nil {
		return writer
	}
	switch w := writer.(type) {
	case *RedisStreamWriter:
		w.clock = clock
	case *APIStreamWriter:
		w.clock = clock
	}
	return &clockStreamWriter{StreamWriter: writer, clock: clock}
}

func (w *clockStreamWriter) WriteEnd(result map[string]any) error {
	w.clock.finishNow()
	return w.StreamWriter.WriteEnd(result)
}

func (w *clockStreamWriter) WriteError(err error, recoverable bool) error {
	w.clock.finishNow()
	return w.StreamWriter.WriteError(err, recoverable)
}

func (w *clockStreamWriter) WriteCancelled(reason string) error {
	w.clock.finishNow()
	return w.StreamWriter.WriteCancelled(reason)
}
