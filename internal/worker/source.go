package worker

import "context"

// JobSource defines the interface for fetching jobs from a queue/stream.
// The primary implementation is RedisSource (Redis Streams).
type JobSource interface {
	// Name returns the source identifier (e.g., "nexus", "redis")
	Name() string

	// Connect establishes connection to the job source.
	// This should be called before Next().
	Connect(ctx context.Context) error

	// Next blocks until a job is available or context is cancelled.
	// Returns nil job (no error) if no job is available within timeout.
	// The job is "claimed" by this worker and should be Ack'd or Nack'd.
	Next(ctx context.Context) (*Job, error)

	// Ack acknowledges successful job completion.
	// The job will be removed from the queue.
	Ack(ctx context.Context, job *Job) error

	// Nack indicates job failure.
	// Depending on implementation, job may be retried or moved to DLQ.
	Nack(ctx context.Context, job *Job, err error) error

	// Close cleanly disconnects from the job source.
	Close() error
}

// SourceConfig is the common configuration for job sources.
type SourceConfig struct {
	// Name is the source identifier
	Name string

	// URL is the connection URL (e.g., Nexus URL or Redis URL)
	URL string

	// Queue is the queue/stream name to consume from
	Queue string

	// ConsumerGroup is the consumer group name (for Redis Streams)
	ConsumerGroup string

	// BlockTimeout is how long to wait for a job before returning nil
	BlockTimeout int // milliseconds

	// MaxAttempts is the maximum retry count before DLQ
	MaxAttempts int

	// Auth contains authentication details
	Auth SourceAuth
}

// SourceAuth contains authentication configuration.
type SourceAuth struct {
	// APIKey is a bearer token for authentication
	APIKey string

	// Password is used for Redis authentication
	Password string
}
