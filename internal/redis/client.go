// Package redis provides high-performance Redis Streams and Pub/Sub functionality
// for the Citadel worker job queue system.
//
// This package is designed for high-throughput job routing to AceTeam's private
// GPU cloud infrastructure. Key design choices:
//
//   - Uses Redis Streams with consumer groups for reliable, distributed job processing
//   - Supports horizontal scaling across multiple Citadel worker instances
//   - Publishes streaming responses via Redis Pub/Sub for real-time results
//   - Implements Dead Letter Queue (DLQ) handling for failed jobs
//
// The Citadel worker (Go) handles private GPU infrastructure routing, while the
// Python worker handles lightweight external API calls (OpenAI, Anthropic, etc.).
package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// StreamEvent represents an event published to Redis Pub/Sub for streaming responses.
type StreamEvent struct {
	Version   string                 `json:"version"`
	Type      string                 `json:"type"` // "start", "chunk", "end", "error"
	JobID     string                 `json:"jobId"`
	Timestamp string                 `json:"timestamp"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// Job represents a job read from Redis Streams.
type Job struct {
	MessageID string
	JobID     string
	Type      string
	Payload   map[string]interface{}
	RawData   map[string]interface{}
}

// Client wraps Redis operations for the job queue system.
type Client struct {
	client        *redis.Client
	workerID      string
	queueName     string
	consumerGroup string
	blockMs       int
	maxAttempts   int
}

// ClientConfig holds configuration for the Redis client.
type ClientConfig struct {
	URL           string
	Password      string
	QueueName     string
	ConsumerGroup string
	BlockMs       int
	MaxAttempts   int
}

// NewClient creates a new Redis client for the job queue.
func NewClient(cfg ClientConfig) *Client {
	if cfg.ConsumerGroup == "" {
		cfg.ConsumerGroup = "citadel-workers"
	}
	if cfg.BlockMs == 0 {
		cfg.BlockMs = 5000 // 5 seconds default
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 3
	}

	return &Client{
		workerID:      fmt.Sprintf("citadel-%s", uuid.New().String()[:8]),
		queueName:     cfg.QueueName,
		consumerGroup: cfg.ConsumerGroup,
		blockMs:       cfg.BlockMs,
		maxAttempts:   cfg.MaxAttempts,
	}
}

// Connect establishes connection to Redis.
func (c *Client) Connect(ctx context.Context, url, password string) error {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return fmt.Errorf("failed to parse Redis URL: %w", err)
	}

	if password != "" {
		opts.Password = password
	}

	c.client = redis.NewClient(opts)

	// Verify connection
	if err := c.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return nil
}

// EnsureConsumerGroup creates the consumer group if it doesn't exist.
func (c *Client) EnsureConsumerGroup(ctx context.Context) error {
	// Try to create consumer group from beginning of stream
	err := c.client.XGroupCreateMkStream(ctx, c.queueName, c.consumerGroup, "0").Err()
	if err != nil {
		// Ignore "BUSYGROUP" error (group already exists)
		if !strings.Contains(err.Error(), "BUSYGROUP") {
			return fmt.Errorf("failed to create consumer group: %w", err)
		}
	}
	return nil
}

// EnsureConsumerGroups creates consumer groups for multiple queues.
func (c *Client) EnsureConsumerGroups(ctx context.Context, queues []string) error {
	for _, queue := range queues {
		err := c.client.XGroupCreateMkStream(ctx, queue, c.consumerGroup, "0").Err()
		if err != nil {
			if !strings.Contains(err.Error(), "BUSYGROUP") {
				return fmt.Errorf("failed to create consumer group for %s: %w", queue, err)
			}
		}
	}
	return nil
}

// ReadJob reads the next available job from the stream using XREADGROUP.
// Returns nil if no job is available within the block timeout.
func (c *Client) ReadJob(ctx context.Context) (*Job, error) {
	streams, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.consumerGroup,
		Consumer: c.workerID,
		Streams:  []string{c.queueName, ">"},
		Count:    1,
		Block:    time.Duration(c.blockMs) * time.Millisecond,
	}).Result()

	if err != nil {
		if err == redis.Nil {
			return nil, nil // No message available
		}
		return nil, fmt.Errorf("failed to read from stream: %w", err)
	}

	if len(streams) == 0 || len(streams[0].Messages) == 0 {
		return nil, nil
	}

	msg := streams[0].Messages[0]
	return c.parseMessage(msg)
}

// ReadJobMulti reads the next available job from multiple streams using XREADGROUP.
// Returns the job and the queue it came from, or nil if no job is available.
func (c *Client) ReadJobMulti(ctx context.Context, queues []string) (*Job, string, error) {
	if len(queues) == 0 {
		return nil, "", fmt.Errorf("no queues specified")
	}

	// Build streams arg: [queue1, queue2, ..., ">", ">", ...]
	streamArgs := make([]string, 0, len(queues)*2)
	streamArgs = append(streamArgs, queues...)
	for range queues {
		streamArgs = append(streamArgs, ">")
	}

	streams, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.consumerGroup,
		Consumer: c.workerID,
		Streams:  streamArgs,
		Count:    1,
		Block:    time.Duration(c.blockMs) * time.Millisecond,
	}).Result()

	if err != nil {
		if err == redis.Nil {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("failed to read from streams: %w", err)
	}

	for _, stream := range streams {
		if len(stream.Messages) > 0 {
			job, err := c.parseMessage(stream.Messages[0])
			if err != nil {
				return nil, "", err
			}
			return job, stream.Stream, nil
		}
	}

	return nil, "", nil
}

// AckJobOnQueue acknowledges a message on a specific queue (for multi-queue mode).
func (c *Client) AckJobOnQueue(ctx context.Context, queue, messageID string) error {
	return c.client.XAck(ctx, queue, c.consumerGroup, messageID).Err()
}

// GetDeliveryCountOnQueue returns the delivery count for a message on a specific queue.
func (c *Client) GetDeliveryCountOnQueue(ctx context.Context, queue, messageID string) (int64, error) {
	pending, err := c.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: queue,
		Group:  c.consumerGroup,
		Start:  messageID,
		End:    messageID,
		Count:  1,
	}).Result()

	if err != nil {
		return 0, err
	}

	if len(pending) > 0 {
		return pending[0].RetryCount, nil
	}

	return 0, nil
}

// MoveToDLQFromQueue moves a failed message to the DLQ, specifying the source queue.
func (c *Client) MoveToDLQFromQueue(ctx context.Context, queue string, job *Job, reason string) error {
	// Build DLQ name preserving full tag context
	// jobs:v1:tag:gpu:rtx4090 -> dlq:v1:tag:gpu:rtx4090
	// jobs:v1:cpu-general -> dlq:v1:cpu-general
	var dlqName string
	if strings.HasPrefix(queue, "jobs:v1:") {
		dlqName = "dlq:v1:" + strings.TrimPrefix(queue, "jobs:v1:")
	} else {
		parts := strings.Split(queue, ":")
		dlqName = fmt.Sprintf("dlq:v1:%s", parts[len(parts)-1])
	}

	fields := map[string]interface{}{
		"original_message_id": job.MessageID,
		"original_queue":      queue,
		"reason":              reason,
		"moved_at":            time.Now().UTC().Format(time.RFC3339),
		"worker_id":           c.workerID,
		"jobId":               job.JobID,
	}

	if payloadBytes, err := json.Marshal(job.Payload); err == nil {
		fields["payload"] = string(payloadBytes)
	}

	return c.client.XAdd(ctx, &redis.XAddArgs{
		Stream: dlqName,
		Values: fields,
	}).Err()
}

// parseMessage converts a Redis stream message to a Job.
func (c *Client) parseMessage(msg redis.XMessage) (*Job, error) {
	job := &Job{
		MessageID: msg.ID,
		RawData:   make(map[string]interface{}),
	}

	// Copy raw data
	for k, v := range msg.Values {
		job.RawData[k] = v
	}

	// Extract jobId
	if jobID, ok := msg.Values["jobId"].(string); ok {
		job.JobID = jobID
	}

	// Extract type
	if jobType, ok := msg.Values["type"].(string); ok {
		job.Type = jobType
	}

	// Parse payload JSON
	if payloadStr, ok := msg.Values["payload"].(string); ok {
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
			return nil, fmt.Errorf("failed to parse job payload: %w", err)
		}
		job.Payload = payload

		// Also extract type from payload if not at top level
		if job.Type == "" {
			if t, ok := payload["type"].(string); ok {
				job.Type = t
			}
		}
	}

	return job, nil
}

// AckJob acknowledges a successfully processed message.
func (c *Client) AckJob(ctx context.Context, messageID string) error {
	return c.client.XAck(ctx, c.queueName, c.consumerGroup, messageID).Err()
}

// GetDeliveryCount returns the number of times a message has been delivered.
func (c *Client) GetDeliveryCount(ctx context.Context, messageID string) (int64, error) {
	pending, err := c.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: c.queueName,
		Group:  c.consumerGroup,
		Start:  messageID,
		End:    messageID,
		Count:  1,
	}).Result()

	if err != nil {
		return 0, err
	}

	if len(pending) > 0 {
		return pending[0].RetryCount, nil
	}

	return 0, nil
}

// MoveToD LQ moves a failed message to the Dead Letter Queue.
func (c *Client) MoveToDLQ(ctx context.Context, job *Job, reason string) error {
	dlqName := c.getDLQName()

	fields := map[string]interface{}{
		"original_message_id": job.MessageID,
		"original_queue":      c.queueName,
		"reason":              reason,
		"moved_at":            time.Now().UTC().Format(time.RFC3339),
		"worker_id":           c.workerID,
		"jobId":               job.JobID,
	}

	// Include original payload
	if payloadBytes, err := json.Marshal(job.Payload); err == nil {
		fields["payload"] = string(payloadBytes)
	}

	return c.client.XAdd(ctx, &redis.XAddArgs{
		Stream: dlqName,
		Values: fields,
	}).Err()
}

// getDLQName returns the Dead Letter Queue name for this queue.
func (c *Client) getDLQName() string {
	// Extract queue suffix (e.g., "gpu-general" from "jobs:v1:gpu-general")
	parts := strings.Split(c.queueName, ":")
	suffix := parts[len(parts)-1]
	return fmt.Sprintf("dlq:v1:%s", suffix)
}

// PublishStreamEvent publishes a streaming event to Redis Pub/Sub.
func (c *Client) PublishStreamEvent(ctx context.Context, jobID string, eventType string, data map[string]interface{}) error {
	streamName := fmt.Sprintf("stream:v1:%s", jobID)

	event := StreamEvent{
		Version:   "1.0",
		Type:      eventType,
		JobID:     jobID,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      data,
	}

	eventJSON, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal stream event: %w", err)
	}

	return c.client.Publish(ctx, streamName, eventJSON).Err()
}

// PublishStart publishes a "start" event for a job.
func (c *Client) PublishStart(ctx context.Context, jobID string, message string) error {
	return c.PublishStreamEvent(ctx, jobID, "start", map[string]interface{}{
		"message": message,
	})
}

// PublishChunk publishes a "chunk" event for streaming responses.
func (c *Client) PublishChunk(ctx context.Context, jobID string, content string, index int) error {
	return c.PublishStreamEvent(ctx, jobID, "chunk", map[string]interface{}{
		"content": content,
		"index":   index,
	})
}

// PublishEnd publishes an "end" event when job completes.
func (c *Client) PublishEnd(ctx context.Context, jobID string, result map[string]interface{}) error {
	return c.PublishStreamEvent(ctx, jobID, "end", map[string]interface{}{
		"result": result,
	})
}

// PublishError publishes an "error" event when job fails.
func (c *Client) PublishError(ctx context.Context, jobID string, errMsg string, recoverable bool) error {
	return c.PublishStreamEvent(ctx, jobID, "error", map[string]interface{}{
		"error":       errMsg,
		"recoverable": recoverable,
	})
}

// SetJobStatus stores job status in Redis (simpler than Supabase for now).
func (c *Client) SetJobStatus(ctx context.Context, jobID, status string, data map[string]interface{}) error {
	key := fmt.Sprintf("job:%s:status", jobID)

	fields := map[string]interface{}{
		"status":     status,
		"worker_id":  c.workerID,
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}

	for k, v := range data {
		fields[k] = v
	}

	return c.client.HSet(ctx, key, fields).Err()
}

// Close closes the Redis connection.
func (c *Client) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// WorkerID returns the unique worker identifier.
func (c *Client) WorkerID() string {
	return c.workerID
}

// QueueName returns the queue name this client is configured for.
func (c *Client) QueueName() string {
	return c.queueName
}

// MaxAttempts returns the maximum retry attempts before DLQ.
func (c *Client) MaxAttempts() int {
	return c.maxAttempts
}
