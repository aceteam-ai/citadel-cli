package worker

import (
	"context"
	"fmt"

	redisclient "github.com/aceboss/citadel-cli/internal/redis"
)

// RedisSource implements JobSource for Redis Streams.
// This is the job source for AceTeam's private GPU cloud infrastructure.
type RedisSource struct {
	client *redisclient.Client
	config RedisSourceConfig
}

// RedisSourceConfig holds configuration for RedisSource.
type RedisSourceConfig struct {
	// URL is the Redis connection URL
	URL string

	// Password is the Redis password (optional)
	Password string

	// QueueName is the Redis Stream to consume from
	QueueName string

	// ConsumerGroup is the consumer group name (default: "citadel-workers")
	ConsumerGroup string

	// BlockMs is how long to wait for a job before returning nil (default: 5000)
	BlockMs int

	// MaxAttempts is the maximum retry count before DLQ (default: 3)
	MaxAttempts int
}

// NewRedisSource creates a new Redis Streams job source.
func NewRedisSource(cfg RedisSourceConfig) *RedisSource {
	if cfg.QueueName == "" {
		cfg.QueueName = "jobs:v1:gpu-general"
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

	return &RedisSource{
		config: cfg,
	}
}

// Name returns the source identifier.
func (s *RedisSource) Name() string {
	return "redis"
}

// Connect establishes connection to Redis.
func (s *RedisSource) Connect(ctx context.Context) error {
	s.client = redisclient.NewClient(redisclient.ClientConfig{
		URL:           s.config.URL,
		Password:      s.config.Password,
		QueueName:     s.config.QueueName,
		ConsumerGroup: s.config.ConsumerGroup,
		BlockMs:       s.config.BlockMs,
		MaxAttempts:   s.config.MaxAttempts,
	})

	if err := s.client.Connect(ctx, s.config.URL, s.config.Password); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	if err := s.client.EnsureConsumerGroup(ctx); err != nil {
		return fmt.Errorf("failed to create consumer group: %w", err)
	}

	fmt.Printf("   - Redis URL: %s\n", s.config.URL)
	fmt.Printf("   - Worker ID: %s\n", s.client.WorkerID())
	fmt.Printf("   - Queue: %s\n", s.client.QueueName())
	fmt.Printf("   - Consumer group: %s\n", s.config.ConsumerGroup)
	return nil
}

// Next blocks until a job is available or context is cancelled.
func (s *RedisSource) Next(ctx context.Context) (*Job, error) {
	redisJob, err := s.client.ReadJob(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read job from Redis: %w", err)
	}

	if redisJob == nil {
		return nil, nil // No job available
	}

	// Check delivery count for DLQ handling
	deliveryCount, _ := s.client.GetDeliveryCount(ctx, redisJob.MessageID)
	if int(deliveryCount) >= s.client.MaxAttempts() {
		fmt.Printf("   - Job %s exceeded max attempts (%d), moving to DLQ\n",
			redisJob.JobID, s.client.MaxAttempts())
		if err := s.client.MoveToDLQ(ctx, redisJob, "Exceeded max retry attempts"); err != nil {
			fmt.Printf("   - Failed to move job to DLQ: %v\n", err)
		}
		s.client.AckJob(ctx, redisJob.MessageID)
		return nil, nil // Return nil to continue to next job
	}

	// Convert to worker.Job
	return s.convertJob(redisJob), nil
}

// convertJob converts a redis.Job to a worker.Job.
func (s *RedisSource) convertJob(rj *redisclient.Job) *Job {
	return &Job{
		ID:        rj.JobID,
		Type:      rj.Type,
		Payload:   rj.Payload,
		Source:    "redis",
		MessageID: rj.MessageID,
	}
}

// Ack acknowledges successful job completion.
func (s *RedisSource) Ack(ctx context.Context, job *Job) error {
	s.client.SetJobStatus(ctx, job.ID, "completed", nil)
	return s.client.AckJob(ctx, job.MessageID)
}

// Nack indicates job failure.
// For Redis, this updates status but does NOT ACK - allowing retry or DLQ.
func (s *RedisSource) Nack(ctx context.Context, job *Job, err error) error {
	s.client.SetJobStatus(ctx, job.ID, "failed", map[string]any{
		"error": err.Error(),
	})
	// Don't ACK - let it retry or go to DLQ on next read
	return nil
}

// Close cleanly disconnects from Redis.
func (s *RedisSource) Close() error {
	if s.client != nil {
		return s.client.Close()
	}
	return nil
}

// Client returns the underlying Redis client for stream writing.
func (s *RedisSource) Client() *redisclient.Client {
	return s.client
}

// Ensure RedisSource implements JobSource
var _ JobSource = (*RedisSource)(nil)
