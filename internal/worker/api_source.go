package worker

import (
	"context"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// APISource implements JobSource using the AceTeam Redis API proxy.
// This is the secure alternative to direct Redis connections.
// Supports consuming from multiple queues by round-robining across them.
type APISource struct {
	client     *redisapi.Client
	config     APISourceConfig
	queueNames []string // resolved list of queues to consume from
	queueIndex int      // round-robin index for multi-queue polling
}

// APISourceConfig holds configuration for APISource.
type APISourceConfig struct {
	// BaseURL is the AceTeam API base URL (e.g., "https://aceteam.ai")
	BaseURL string

	// Token is the device_api_token from device authentication
	Token string

	// QueueName is the Redis Stream to consume from (single queue, backwards compat)
	QueueName string

	// QueueNames is the list of Redis Streams to consume from (multi-queue mode).
	// If set, QueueName is ignored.
	QueueNames []string

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
		queues = []string{"jobs:v1:cpu-general"}
	}

	return &APISource{
		config:     cfg,
		queueNames: queues,
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
// When consuming from multiple queues, polls each queue in round-robin
// with a short block timeout to avoid starving any queue.
func (s *APISource) Next(ctx context.Context) (*Job, error) {
	if len(s.queueNames) == 1 {
		return s.nextSingle(ctx)
	}
	return s.nextMulti(ctx)
}

// nextSingle reads from a single queue (original behavior).
func (s *APISource) nextSingle(ctx context.Context) (*Job, error) {
	apiJob, err := s.client.ConsumeJob(ctx, redisapi.ConsumeRequest{
		Queue:    s.queueNames[0],
		Group:    s.config.ConsumerGroup,
		Consumer: s.client.WorkerID(),
		Count:    1,
		BlockMs:  s.config.BlockMs,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to consume job from API: %w", err)
	}

	if apiJob == nil {
		return nil, nil
	}

	job := s.convertJob(apiJob)
	job.SourceQueue = s.queueNames[0]
	return job, nil
}

// nextMulti round-robins across queues with a shorter block timeout.
// Each poll checks one queue; if empty, advances to the next.
// Individual queue failures (e.g., rejected by server validation) are
// logged and skipped rather than failing the entire poll cycle. Only
// when all queues error does the caller see an error (triggering backoff).
func (s *APISource) nextMulti(ctx context.Context) (*Job, error) {
	// Use a shorter block per queue so we cycle through them all within
	// roughly the configured block timeout.
	perQueueBlockMs := s.config.BlockMs / len(s.queueNames)
	if perQueueBlockMs < 500 {
		perQueueBlockMs = 500
	}

	var lastErr error
	errCount := 0

	for i := 0; i < len(s.queueNames); i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		queue := s.queueNames[s.queueIndex]
		s.queueIndex = (s.queueIndex + 1) % len(s.queueNames)

		apiJob, err := s.client.ConsumeJob(ctx, redisapi.ConsumeRequest{
			Queue:    queue,
			Group:    s.config.ConsumerGroup,
			Consumer: s.client.WorkerID(),
			Count:    1,
			BlockMs:  perQueueBlockMs,
		})
		if err != nil {
			// Log and skip -- one rejected queue must not block the others.
			s.log("warning", "consume failed on %s: %v", queue, err)
			lastErr = err
			errCount++
			continue
		}

		if apiJob != nil {
			job := s.convertJob(apiJob)
			job.SourceQueue = queue
			return job, nil
		}
	}

	// Only propagate error (triggering backoff) if ALL queues failed.
	if errCount == len(s.queueNames) {
		return nil, fmt.Errorf("all queues failed: %w", lastErr)
	}

	return nil, nil // No job available on any queue
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
	queue := job.SourceQueue
	if queue == "" {
		queue = s.queueNames[0]
	}
	return s.client.AcknowledgeJob(ctx, redisapi.AcknowledgeRequest{
		Queue:     queue,
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

// QueueNames returns the list of queues being consumed.
func (s *APISource) QueueNames() []string {
	return s.queueNames
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
