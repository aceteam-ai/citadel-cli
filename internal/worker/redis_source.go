package worker

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

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
	client *redisclient.Client
	config RedisSourceConfig

	// mu guards queueNames, which is read by the run loop (Next) and may be
	// appended to at runtime by AddQueue (e.g. /agent/resubscribe, issue #236).
	mu         sync.RWMutex
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
	queues := s.snapshotQueues()
	if len(queues) == 1 {
		return s.nextSingle(ctx, queues[0])
	}
	return s.nextMulti(ctx, queues)
}

// snapshotQueues returns a stable copy of the queue list for one poll cycle,
// so concurrent AddQueue calls (e.g. /agent/resubscribe) don't race the loop.
func (s *RedisSource) snapshotQueues() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.queueNames...)
}

// nextSingle reads from a single queue (original behavior).
func (s *RedisSource) nextSingle(ctx context.Context, queue string) (*Job, error) {
	redisJob, err := s.client.ReadJob(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read job from Redis: %w", err)
	}

	if redisJob == nil {
		return nil, nil
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
		return nil, nil
	}

	job := s.convertJob(redisJob)
	job.SourceQueue = queue
	return job, nil
}

// nextMulti reads from multiple queues simultaneously.
func (s *RedisSource) nextMulti(ctx context.Context, queues []string) (*Job, error) {
	redisJob, sourceQueue, err := s.client.ReadJobMulti(ctx, queues)
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
	job := &Job{
		ID:        rj.JobID,
		Type:      rj.Type,
		Payload:   rj.Payload,
		Source:    "redis",
		MessageID: rj.MessageID,
	}
	// Extract rayId: check RawData first (top-level stream field), then payload
	if rayID, ok := rj.RawData["rayId"].(string); ok && rayID != "" {
		job.RayID = rayID
	} else if rj.Payload != nil {
		if rayID, ok := rj.Payload["rayId"].(string); ok {
			job.RayID = rayID
		}
	}
	return job
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

// Fail is a terminal failure: record "failed" status (with structured data) and
// ACK the message so it is removed from the consumer group's PEL. Used for
// failures that will never succeed on retry (e.g. an unsupported job type),
// preventing the message from being redelivered and re-failing forever.
func (s *RedisSource) Fail(ctx context.Context, job *Job, err error, data map[string]any) error {
	status := map[string]any{"error": err.Error()}
	for k, v := range data {
		status[k] = v
	}
	s.client.SetJobStatus(ctx, job.ID, "failed", status)
	if job.SourceQueue != "" {
		return s.client.AckJobOnQueue(ctx, job.SourceQueue, job.MessageID)
	}
	return s.client.AckJob(ctx, job.MessageID)
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
	return s.snapshotQueues()
}

// AddQueue appends an additional queue to consume from after Connect.
//
// Used to subscribe to the worker's per-node shell stream once the node's
// Headscale ID is known (issue #3914), which happens after the source is built
// and connected. The consumer group (and stream, via MKSTREAM) is created
// immediately so the platform dispatcher's consumer-presence check can see the
// stream; the consumer itself registers on the next multi-queue XREADGROUP.
// Guarded by mu so it is safe to call at runtime (e.g. /agent/resubscribe,
// issue #236) concurrently with the run loop, which reads via snapshotQueues.
// A blank or already-present queue is ignored. Returns an error only if the
// consumer group cannot be created.
func (s *RedisSource) AddQueue(ctx context.Context, queue string) error {
	if queue == "" {
		return nil
	}
	s.mu.RLock()
	for _, q := range s.queueNames {
		if q == queue {
			s.mu.RUnlock()
			return nil
		}
	}
	s.mu.RUnlock()
	if s.client != nil {
		if err := s.client.EnsureConsumerGroups(ctx, []string{queue}); err != nil {
			return fmt.Errorf("failed to create consumer group for %s: %w", queue, err)
		}
	}
	s.mu.Lock()
	s.queueNames = append(s.queueNames, queue)
	s.mu.Unlock()
	s.log("info", "   - Added queue: %s", queue)
	return nil
}

// IsJobCancelled checks whether a job has been cancelled by the producer.
func (s *RedisSource) IsJobCancelled(ctx context.Context, jobID string) bool {
	cancelled, err := s.client.IsJobCancelled(ctx, jobID)
	if err != nil {
		s.log("warning", "Failed to check cancellation for %s: %v", jobID, err)
		return false
	}
	return cancelled
}

// Ensure RedisSource implements JobSource
var _ JobSource = (*RedisSource)(nil)
