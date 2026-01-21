package worker

import (
	"context"

	redisclient "github.com/aceteam-ai/citadel-cli/internal/redis"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
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

// APIStreamWriter implements StreamWriter using the Redis API proxy.
// This enables real-time streaming of job output via the secure API.
type APIStreamWriter struct {
	client *redisapi.Client
	jobID  string
	ctx    context.Context
}

// NewAPIStreamWriter creates a new API-backed stream writer.
func NewAPIStreamWriter(ctx context.Context, client *redisapi.Client, jobID string) *APIStreamWriter {
	return &APIStreamWriter{
		client: client,
		jobID:  jobID,
		ctx:    ctx,
	}
}

// WriteStart signals the beginning of job processing.
func (w *APIStreamWriter) WriteStart(message string) error {
	w.client.SetJobStatus(w.ctx, w.jobID, "processing", nil)
	return w.client.PublishStart(w.ctx, w.jobID, message)
}

// WriteChunk sends an incremental output chunk (e.g., LLM token).
func (w *APIStreamWriter) WriteChunk(content string, index int) error {
	return w.client.PublishChunk(w.ctx, w.jobID, content, index)
}

// WriteEnd signals successful job completion with final result.
func (w *APIStreamWriter) WriteEnd(result map[string]any) error {
	return w.client.PublishEnd(w.ctx, w.jobID, result)
}

// WriteError signals job failure.
func (w *APIStreamWriter) WriteError(err error, recoverable bool) error {
	return w.client.PublishError(w.ctx, w.jobID, err.Error(), recoverable)
}

// Ensure APIStreamWriter implements StreamWriter
var _ StreamWriter = (*APIStreamWriter)(nil)

// CreateAPIStreamWriterFactory returns a factory function for creating API stream writers.
// This is used with Runner.WithStreamWriterFactory().
func CreateAPIStreamWriterFactory(ctx context.Context, source *APISource) func(jobID string) StreamWriter {
	return func(jobID string) StreamWriter {
		return NewAPIStreamWriter(ctx, source.Client(), jobID)
	}
}
