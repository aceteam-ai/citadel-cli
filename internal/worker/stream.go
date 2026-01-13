package worker

import (
	"context"

	redisclient "github.com/aceboss/citadel-cli/internal/redis"
)

// RedisStreamWriter implements StreamWriter using Redis Pub/Sub.
// This enables real-time streaming of job output to subscribers.
type RedisStreamWriter struct {
	client *redisclient.Client
	jobID  string
	ctx    context.Context
}

// NewRedisStreamWriter creates a new Redis-backed stream writer.
func NewRedisStreamWriter(ctx context.Context, client *redisclient.Client, jobID string) *RedisStreamWriter {
	return &RedisStreamWriter{
		client: client,
		jobID:  jobID,
		ctx:    ctx,
	}
}

// WriteStart signals the beginning of job processing.
func (w *RedisStreamWriter) WriteStart(message string) error {
	w.client.SetJobStatus(w.ctx, w.jobID, "processing", nil)
	return w.client.PublishStart(w.ctx, w.jobID, message)
}

// WriteChunk sends an incremental output chunk (e.g., LLM token).
func (w *RedisStreamWriter) WriteChunk(content string, index int) error {
	return w.client.PublishChunk(w.ctx, w.jobID, content, index)
}

// WriteEnd signals successful job completion with final result.
func (w *RedisStreamWriter) WriteEnd(result map[string]any) error {
	return w.client.PublishEnd(w.ctx, w.jobID, result)
}

// WriteError signals job failure.
func (w *RedisStreamWriter) WriteError(err error, recoverable bool) error {
	return w.client.PublishError(w.ctx, w.jobID, err.Error(), recoverable)
}

// Ensure RedisStreamWriter implements StreamWriter
var _ StreamWriter = (*RedisStreamWriter)(nil)

// CreateRedisStreamWriterFactory returns a factory function for creating Redis stream writers.
// This is used with Runner.WithStreamWriterFactory().
func CreateRedisStreamWriterFactory(ctx context.Context, source *RedisSource) func(jobID string) StreamWriter {
	return func(jobID string) StreamWriter {
		return NewRedisStreamWriter(ctx, source.Client(), jobID)
	}
}
