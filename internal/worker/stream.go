package worker

import (
	"context"
	"encoding/json"
	"strings"
	"time"

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
	// retain also stores terminal events under TerminalResultKey.
	retain bool
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

// WriteReasoningChunk keeps reasoning out of the answer-content stream.
func (w *RedisStreamWriter) WriteReasoningChunk(content string, index int) error {
	return w.client.PublishStreamEvent(w.ctx, w.jobID, w.rayID, "chunk", map[string]any{
		"reasoning_content": content,
		"index":             index,
	})
}

// WriteEnd signals successful job completion with final result.
func (w *RedisStreamWriter) WriteEnd(result map[string]any) error {
	return w.publishTerminal("end", w.clock.data(map[string]any{"result": result}, true))
}

// WriteError signals job failure.
func (w *RedisStreamWriter) WriteError(err error, recoverable bool) error {
	return w.publishTerminal("error", w.clock.data(map[string]any{"error": err.Error(), "recoverable": recoverable}, true))
}

// WriteCancelled signals job cancellation.
func (w *RedisStreamWriter) WriteCancelled(reason string) error {
	w.client.SetJobStatus(w.ctx, w.jobID, "cancelled", nil)
	return w.publishTerminal("cancelled", w.clock.data(map[string]any{"reason": reason}, true))
}

// publishTerminal publishes a terminal event and, for a retaining writer, then
// stores the same envelope under TerminalResultKey. Only the publish error is
// returned, so a retention failure never triggers a publish retry.
func (w *RedisStreamWriter) publishTerminal(eventType string, data map[string]any) error {
	eventJSON, err := w.client.StreamEventJSON(w.jobID, w.rayID, eventType, data)
	if err != nil {
		return err
	}
	publishErr := w.client.PublishStreamEventJSON(w.ctx, w.jobID, eventJSON)
	if w.retain {
		retainTerminalEvent(w.ctx, w.client, w.jobID, eventJSON)
	}
	return publishErr
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
		return redisStreamWriterFor(ctx, source.Client(), job)
	}
}

// redisStreamWriterFor builds a job's direct-Redis writer, retaining terminal
// events for the job types retainsTerminalResult names.
func redisStreamWriterFor(ctx context.Context, client *redisclient.Client, job *Job) *RedisStreamWriter {
	w := NewRedisStreamWriter(ctx, client, job.ID, job.RayID)
	w.retain = retainsTerminalResult(job.Type)
	return w
}

// APIStreamWriter implements StreamWriter using the Redis API proxy.
// This enables real-time streaming of job output via the secure API.
type APIStreamWriter struct {
	durable bool
	clock   *deliveryClock
	client  *redisapi.Client
	jobID   string
	rayID   string
	ctx     context.Context
	// retainer, when set, stores terminal events through the scoped
	// POST /api/fabric/redis/jobs/{jobId}/result route (citadel-cli#1553).
	// A durable delivery commits its terminal envelope itself and never retains.
	retainer terminalRetainer
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
	if !w.durable {
		w.client.SetJobStatus(w.ctx, w.jobID, "processing", nil)
	}
	return w.client.PublishStart(w.ctx, w.jobID, w.rayID, message)
}

// WriteChunk sends an incremental output chunk (e.g., LLM token).
func (w *APIStreamWriter) WriteChunk(content string, index int) error {
	return w.client.PublishChunk(w.ctx, w.jobID, w.rayID, content, index)
}

// WriteReasoningChunk keeps reasoning out of the answer-content stream.
func (w *APIStreamWriter) WriteReasoningChunk(content string, index int) error {
	return w.client.PublishStreamEvent(w.ctx, w.jobID, w.rayID, "chunk", map[string]any{
		"reasoning_content": content,
		"index":             index,
	})
}

// WriteEnd signals successful job completion with final result.
func (w *APIStreamWriter) WriteEnd(result map[string]any) error {
	return w.publishTerminal("end", w.clock.data(map[string]any{"result": result}, true))
}

// WriteError signals job failure.
func (w *APIStreamWriter) WriteError(err error, recoverable bool) error {
	return w.publishTerminal("error", w.clock.data(map[string]any{"error": err.Error(), "recoverable": recoverable}, true))
}

// WriteCancelled signals job cancellation.
func (w *APIStreamWriter) WriteCancelled(reason string) error {
	if !w.durable {
		w.client.SetJobStatus(w.ctx, w.jobID, "cancelled", nil)
	}
	return w.publishTerminal("cancelled", w.clock.data(map[string]any{"reason": reason}, true))
}

// publishTerminal publishes a terminal event and, for a non-durable writer with
// a retainer, then stores the same envelope under TerminalResultKey. Only the
// publish error is returned.
func (w *APIStreamWriter) publishTerminal(eventType string, data map[string]any) error {
	event := w.client.BuildStreamEvent(w.jobID, w.rayID, eventType, data)
	publishErr := w.client.PublishEvent(w.ctx, event)
	if w.retainer != nil && !w.durable {
		if eventJSON, err := json.Marshal(event); err != nil {
			retentionLogf("job %s: retaining terminal result failed (publish unaffected): %v", w.jobID, err)
		} else {
			retainTerminalEvent(w.ctx, w.retainer, w.jobID, eventJSON)
		}
	}
	return publishErr
}

// WriteProgress publishes a throttled progress event via the API proxy and
// mirrors the latest snapshot into the bounded job:<id>:progress key. The mirror
// is best-effort; the pub/sub event is the primary path.
func (w *APIStreamWriter) WriteProgress(event map[string]any) error {
	if !w.durable {
		_ = w.client.SetJobProgress(w.ctx, w.jobID, event)
	}
	return w.client.PublishStreamEvent(w.ctx, w.jobID, w.rayID, "progress", event)
}

// Ensure APIStreamWriter implements StreamWriter
var _ StreamWriter = (*APIStreamWriter)(nil)

// CreateAPIStreamWriterFactory returns a factory function for creating API stream writers.
// This is used with Runner.WithStreamWriterFactory().
func CreateAPIStreamWriterFactory(ctx context.Context, source *APISource) func(job *Job) StreamWriter {
	return func(job *Job) StreamWriter {
		return apiStreamWriterFor(ctx, source.Client(), job)
	}
}

// apiStreamWriterFor builds a job's API-proxy writer. API-mode retention is
// on for the job types retainsTerminalResult names.
func apiStreamWriterFor(ctx context.Context, client *redisapi.Client, job *Job) *APIStreamWriter {
	w := NewAPIStreamWriter(ctx, client, job.ID, job.RayID)
	if retainsTerminalResult(job.Type) {
		w.retainer = apiTerminalRetainer(client)
	}
	return w
}

// apiTerminalRetainer is the API-mode retention seam: it posts the terminal
// envelope to the scoped POST /api/fabric/redis/jobs/{jobId}/result route
// through the node's existing proxy client. A package var so tests can swap it.
var apiTerminalRetainer = func(c *redisapi.Client) terminalRetainer { return apiResultRetainer{c} }

// apiResultRetainer adapts redisapi.Client to terminalRetainer. The interface
// carries the direct-mode key, so the job id is recovered from it.
type apiResultRetainer struct{ client *redisapi.Client }

func (r apiResultRetainer) RetainTerminalEvent(ctx context.Context, key string, eventJSON []byte, ttl time.Duration) (bool, error) {
	jobID := strings.TrimSuffix(strings.TrimPrefix(key, "job:"), ":result")
	return r.client.RetainTerminalResult(ctx, jobID, eventJSON, ttl)
}

// clockStreamWriter observes existing carriers without creating any event or
// changing the handler/client interface. Test/custom writers still see exactly
// their original calls; real writers add metadata beside unchanged result data.
type clockStreamWriter struct {
	StreamWriter
	clock *deliveryClock
}

// Preserve optional reasoning delivery through the runner's clock wrapper.
// Without this forwarding method, the LLM handler sees only the wrapper and
// drops reasoning chunks even when the underlying writer supports them.
func (w *clockStreamWriter) WriteReasoningChunk(content string, index int) error {
	if writer, ok := w.StreamWriter.(reasoningChunkWriter); ok {
		return writer.WriteReasoningChunk(content, index)
	}
	return nil
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
