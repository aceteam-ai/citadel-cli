package worker

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	redisclient "github.com/aceteam-ai/citadel-cli/internal/redis"
)

// maskRedisURL masks the password in a Redis URL for safe logging.
// redis://:password@host:port -> redis://***@host:port
func maskRedisURL(redisURL string) string {
	u, err := url.Parse(redisURL)
	if err != nil {
		// If parsing fails, just show the scheme and a placeholder
		if strings.HasPrefix(redisURL, "redis://") {
			return "redis://***"
		}
		return "***"
	}
	// If there's a password, mask it
	if _, hasPass := u.User.Password(); hasPass {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return u.String()
}

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

	// LogFn is an optional callback for logging (if nil, prints to stdout)
	LogFn func(level, msg string)
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

// log outputs a message - uses LogFn callback if set, otherwise prints to stdout/stderr.
func (s *RedisSource) log(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if s.config.LogFn != nil {
		s.config.LogFn(level, msg)
	} else {
		fmt.Printf("%s\n", msg)
	}
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

	s.log("info", "   - Redis: %s", maskRedisURL(s.config.URL))
	s.log("info", "   - Worker ID: %s", s.client.WorkerID())
	s.log("info", "   - Queue: %s", s.client.QueueName())
	s.log("info", "   - Consumer group: %s", s.config.ConsumerGroup)
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
		s.log("warning", "   - Job %s exceeded max attempts (%d), moving to DLQ",
			redisJob.JobID, s.client.MaxAttempts())
		if err := s.client.MoveToDLQ(ctx, redisJob, "Exceeded max retry attempts"); err != nil {
			s.log("error", "   - Failed to move job to DLQ: %v", err)
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
