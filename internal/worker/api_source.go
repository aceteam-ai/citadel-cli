package worker

import (
	"context"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// APISource implements JobSource using the AceTeam Redis API proxy.
// This is the secure alternative to direct Redis connections.
type APISource struct {
	client *redisapi.Client
	config APISourceConfig
}

// APISourceConfig holds configuration for APISource.
type APISourceConfig struct {
	// BaseURL is the AceTeam API base URL (e.g., "https://aceteam.ai")
	BaseURL string

	// Token is the device_api_token from device authentication
	Token string

	// QueueName is the Redis Stream to consume from
	QueueName string

	// ConsumerGroup is the consumer group name (default: "citadel-workers")
	ConsumerGroup string

	// BlockMs is how long to wait for a job before returning nil (default: 5000)
	BlockMs int

	// MaxAttempts is the maximum retry count before DLQ (default: 3)
	MaxAttempts int

	// DebugFunc is an optional callback for debug logging
	DebugFunc func(format string, args ...any)

	// LogFn is an optional callback for logging (if nil, prints to stdout)
	LogFn func(level, msg string)
}

// NewAPISource creates a new API-backed job source.
func NewAPISource(cfg APISourceConfig) *APISource {
	if cfg.QueueName == "" {
		cfg.QueueName = "jobs:v1:cpu-general"
	}
	if cfg.ConsumerGroup == "" {
		cfg.ConsumerGroup = "citadel-workers"
	}
	if cfg.BlockMs == 0 {
		cfg.BlockMs = 5000
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 3
	}

	return &APISource{
		config: cfg,
	}
}

// Name returns the source identifier.
func (s *APISource) Name() string {
	return "redis-api"
}

// log outputs a message - uses LogFn callback if set, otherwise prints to stdout.
func (s *APISource) log(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if s.config.LogFn != nil {
		s.config.LogFn(level, msg)
	} else {
		fmt.Printf("%s\n", msg)
	}
}

// Connect establishes connection to the API.
func (s *APISource) Connect(ctx context.Context) error {
	// Skip if already connected
	if s.client != nil {
		return nil
	}

	s.client = redisapi.NewClient(redisapi.ClientConfig{
		BaseURL:   s.config.BaseURL,
		Token:     s.config.Token,
		DebugFunc: s.config.DebugFunc,
	})

	// Verify connection
	if err := s.client.Ping(ctx); err != nil {
		return fmt.Errorf("failed to connect to Redis API: %w", err)
	}

	s.log("info", "   - API: %s", s.config.BaseURL)
	s.log("info", "   - Worker ID: %s", s.client.WorkerID())
	s.log("info", "   - Queue: %s", s.config.QueueName)
	s.log("info", "   - Consumer group: %s", s.config.ConsumerGroup)
	return nil
}

// Next blocks until a job is available or context is cancelled.
func (s *APISource) Next(ctx context.Context) (*Job, error) {
	apiJob, err := s.client.ConsumeJob(ctx, redisapi.ConsumeRequest{
		Queue:    s.config.QueueName,
		Group:    s.config.ConsumerGroup,
		Consumer: s.client.WorkerID(),
		Count:    1,
		BlockMs:  s.config.BlockMs,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to consume job from API: %w", err)
	}

	if apiJob == nil {
		return nil, nil // No job available
	}

	// Convert to worker.Job
	return s.convertJob(apiJob), nil
}

// convertJob converts an API job to a worker.Job.
func (s *APISource) convertJob(aj *redisapi.Job) *Job {
	job := &Job{
		ID:        aj.JobID,
		Type:      aj.Type,
		Payload:   aj.Payload,
		Source:    "redis-api",
		MessageID: aj.MessageID,
	}
	// Extract rayId: check RawData first, then payload
	if aj.RawData != nil {
		if rayID, ok := aj.RawData["rayId"].(string); ok && rayID != "" {
			job.RayID = rayID
		}
	}
	if job.RayID == "" && aj.Payload != nil {
		if rayID, ok := aj.Payload["rayId"].(string); ok {
			job.RayID = rayID
		}
	}
	return job
}

// Ack acknowledges successful job completion.
func (s *APISource) Ack(ctx context.Context, job *Job) error {
	s.client.SetJobStatus(ctx, job.ID, "completed", nil)
	return s.client.AcknowledgeJob(ctx, redisapi.AcknowledgeRequest{
		Queue:     s.config.QueueName,
		Group:     s.config.ConsumerGroup,
		MessageID: job.MessageID,
	})
}

// Nack indicates job failure.
// For the API, this updates status but does NOT ACK - allowing retry.
func (s *APISource) Nack(ctx context.Context, job *Job, err error) error {
	s.client.SetJobStatus(ctx, job.ID, "failed", map[string]any{
		"error": err.Error(),
	})
	// Don't ACK - let it retry
	return nil
}

// Close cleanly disconnects from the API.
func (s *APISource) Close() error {
	if s.client != nil {
		return s.client.Close()
	}
	return nil
}

// Client returns the underlying API client for stream writing.
func (s *APISource) Client() *redisapi.Client {
	return s.client
}

// IsJobCancelled checks whether a job has been cancelled by the producer.
func (s *APISource) IsJobCancelled(ctx context.Context, jobID string) bool {
	cancelled, err := s.client.IsJobCancelled(ctx, jobID)
	if err != nil {
		// Log but don't block — treat check failure as "not cancelled"
		s.log("warning", "Failed to check cancellation for %s: %v", jobID, err)
		return false
	}
	return cancelled
}

// Ensure APISource implements JobSource
var _ JobSource = (*APISource)(nil)
