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
// Supports consuming from multiple queues simultaneously for tag-based routing.
type RedisSource struct {
	client     *redisclient.Client
	config     RedisSourceConfig
	queueNames []string // resolved list of queues to consume from
}

// RedisSourceConfig holds configuration for RedisSource.
type RedisSourceConfig struct {
	// URL is the Redis connection URL
	URL string

	// Password is the Redis password (optional)
	Password string

	// QueueName is the Redis Stream to consume from (single queue, backwards compat)
	QueueName string

	// QueueNames is the list of Redis Streams to consume from (multi-queue mode)
	// If set, QueueName is ignored.
	QueueNames []string

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
	if cfg.ConsumerGroup == "" {
		cfg.ConsumerGroup = "citadel-workers"
	}
	if cfg.BlockMs == 0 {
		cfg.BlockMs = 5000
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 3
	}

	// Resolve queue names: prefer QueueNames, fall back to QueueName
	var queues []string
	if len(cfg.QueueNames) > 0 {
		queues = cfg.QueueNames
	} else if cfg.QueueName != "" {
		queues = []string{cfg.QueueName}
	} else {
		queues = []string{"jobs:v1:gpu-general"}
	}

	return &RedisSource{
		config:     cfg,
		queueNames: queues,
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
	// Use first queue as the "primary" for the client config (backwards compat)
	s.client = redisclient.NewClient(redisclient.ClientConfig{
		URL:           s.config.URL,
		Password:      s.config.Password,
		QueueName:     s.queueNames[0],
		ConsumerGroup: s.config.ConsumerGroup,
		BlockMs:       s.config.BlockMs,
		MaxAttempts:   s.config.MaxAttempts,
	})

	if err := s.client.Connect(ctx, s.config.URL, s.config.Password); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	// Create consumer groups for all queues
	if err := s.client.EnsureConsumerGroups(ctx, s.queueNames); err != nil {
		return fmt.Errorf("failed to create consumer groups: %w", err)
	}

	s.log("info", "   - Redis: %s", maskRedisURL(s.config.URL))
	s.log("info", "   - Worker ID: %s", s.client.WorkerID())
	if len(s.queueNames) == 1 {
		s.log("info", "   - Queue: %s", s.queueNames[0])
	} else {
		s.log("info", "   - Queues (%d):", len(s.queueNames))
		for _, q := range s.queueNames {
			s.log("info", "     - %s", q)
		}
	}
	s.log("info", "   - Consumer group: %s", s.config.ConsumerGroup)
	return nil
}

// Next blocks until a job is available or context is cancelled.
func (s *RedisSource) Next(ctx context.Context) (*Job, error) {
	if len(s.queueNames) == 1 {
		return s.nextSingle(ctx)
	}
	return s.nextMulti(ctx)
}

// nextSingle reads from a single queue (original behavior).
func (s *RedisSource) nextSingle(ctx context.Context) (*Job, error) {
	redisJob, err := s.client.ReadJob(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read job from Redis: %w", err)
	}

	if redisJob == nil {
		return nil, nil
	}

	queue := s.queueNames[0]

	// Check delivery count for DLQ handling
	deliveryCount, _ := s.client.GetDeliveryCount(ctx, redisJob.MessageID)
	if int(deliveryCount) >= s.client.MaxAttempts() {
		s.log("warning", "   - Job %s exceeded max attempts (%d), moving to DLQ",
			redisJob.JobID, s.client.MaxAttempts())
		if err := s.client.MoveToDLQ(ctx, redisJob, "Exceeded max retry attempts"); err != nil {
			s.log("error", "   - Failed to move job to DLQ: %v", err)
		}
		s.client.AckJob(ctx, redisJob.MessageID)
		return nil, nil
	}

	job := s.convertJob(redisJob)
	job.SourceQueue = queue
	return job, nil
}

// nextMulti reads from multiple queues simultaneously.
func (s *RedisSource) nextMulti(ctx context.Context) (*Job, error) {
	redisJob, sourceQueue, err := s.client.ReadJobMulti(ctx, s.queueNames)
	if err != nil {
		return nil, fmt.Errorf("failed to read job from Redis: %w", err)
	}

	if redisJob == nil {
		return nil, nil
	}

	// Check delivery count for DLQ handling
	deliveryCount, _ := s.client.GetDeliveryCountOnQueue(ctx, sourceQueue, redisJob.MessageID)
	if int(deliveryCount) >= s.client.MaxAttempts() {
		s.log("warning", "   - Job %s exceeded max attempts (%d), moving to DLQ",
			redisJob.JobID, s.client.MaxAttempts())
		if err := s.client.MoveToDLQFromQueue(ctx, sourceQueue, redisJob, "Exceeded max retry attempts"); err != nil {
			s.log("error", "   - Failed to move job to DLQ: %v", err)
		}
		s.client.AckJobOnQueue(ctx, sourceQueue, redisJob.MessageID)
		return nil, nil
	}

	job := s.convertJob(redisJob)
	job.SourceQueue = sourceQueue
	return job, nil
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
	if job.SourceQueue != "" {
		return s.client.AckJobOnQueue(ctx, job.SourceQueue, job.MessageID)
	}
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

// QueueNames returns the list of queues being consumed.
func (s *RedisSource) QueueNames() []string {
	return s.queueNames
}

// Ensure RedisSource implements JobSource
var _ JobSource = (*RedisSource)(nil)
