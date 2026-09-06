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
	Type      string                 `json:"type"` // "start", "chunk", "end", "error", "cancelled"
	JobID     string                 `json:"jobId"`
	RayID     string                 `json:"rayId,omitempty"`
	Timestamp string                 `json:"timestamp"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// Job represents a job read from Redis Streams.
type Job struct {
	MessageID  string
	JobID      string
	Type       string
	Payload    map[string]interface{}
	RawData    map[string]interface{}
	EnqueuedAt string // JQS-Core: original enqueue timestamp, preserved for DLQ
}

// NodeMeta holds node identity metadata injected into every stream event.
type NodeMeta struct {
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
}

// Client wraps Redis operations for the job queue system.
type Client struct {
	client        *redis.Client
	workerID      string
	queueName     string
	consumerGroup string
	blockMs       int
	maxAttempts   int
	nodeMeta      *NodeMeta

	// reclaimMinIdle is the effective floor ReclaimStalePendingOnQueue uses.
	// Defaults to the package var StalePendingReclaimMinIdle at construction
	// time (a safe, self-contained default for a caller with no better
	// signal), but internal/worker.RedisSource.Connect overrides it via
	// SetStalePendingReclaimMinIdle with an env-aware, watchdog-derived value
	// -- see that setter's doc comment for why this package cannot compute
	// that value itself.
	reclaimMinIdle time.Duration
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
		workerID:       fmt.Sprintf("citadel-%s", uuid.New().String()[:8]),
		queueName:      cfg.QueueName,
		consumerGroup:  cfg.ConsumerGroup,
		blockMs:        cfg.BlockMs,
		maxAttempts:    cfg.MaxAttempts,
		reclaimMinIdle: StalePendingReclaimMinIdle,
	}
}

// SetStalePendingReclaimMinIdle overrides this client's reclaim-eligibility
// floor for ReclaimStalePending(OnQueue). This package is a leaf and must
// not import internal/worker (see CLAUDE.md's ConfigDir()/leaf-package
// notes for the identical constraint elsewhere in this codebase), so it
// cannot itself read WORKER_JOB_TIMEOUT_LONG_SECONDS or resolve the
// long-tier watchdog's actual ceiling -- the caller (internal/worker.
// RedisSource.Connect, via ResolveStalePendingReclaimFloor) computes that
// env-aware value and hands it across the package boundary through this
// setter. A zero or negative duration is ignored (keeps the current value)
// rather than disabling the floor outright, since "no floor at all" is not
// a value any caller should be able to reach through this seam.
func (c *Client) SetStalePendingReclaimMinIdle(d time.Duration) {
	if d <= 0 {
		return
	}
	c.reclaimMinIdle = d
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

// NonBlockingMs is the block value that makes a read return immediately (no
// server-side XREADGROUP BLOCK). go-redis omits the BLOCK option when the block
// duration is negative, so a negative millisecond count is a non-blocking read.
// Used by the push-wake drain (issue #7270): on a wake nudge the worker reads
// its per-node stream once, right now, instead of waiting out the poll block.
const NonBlockingMs = -1

// ReadJob reads the next available job from the stream using XREADGROUP.
// Returns nil if no job is available within the block timeout.
func (c *Client) ReadJob(ctx context.Context) (*Job, error) {
	return c.ReadJobBlock(ctx, c.blockMs)
}

// ReadJobBlock is ReadJob with an explicit block timeout (ms). Pass
// NonBlockingMs for an immediate, non-blocking read (issue #7270).
func (c *Client) ReadJobBlock(ctx context.Context, blockMs int) (*Job, error) {
	streams, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.consumerGroup,
		Consumer: c.workerID,
		Streams:  []string{c.queueName, ">"},
		Count:    1,
		Block:    time.Duration(blockMs) * time.Millisecond,
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
	return c.ReadJobMultiBlock(ctx, queues, c.blockMs)
}

// ReadJobMultiBlock is ReadJobMulti with an explicit block timeout (ms). Pass
// NonBlockingMs for an immediate, non-blocking read (issue #7270).
func (c *Client) ReadJobMultiBlock(ctx context.Context, queues []string, blockMs int) (*Job, string, error) {
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
		Block:    time.Duration(blockMs) * time.Millisecond,
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

// StalePendingReclaimMinIdle is the package-level DEFAULT for how long a
// delivered-but-unacknowledged message must sit idle in the consumer
// group's pending-entries list (PEL) before ReclaimStalePending(OnQueue)
// will steal it via XAUTOCLAIM and make it eligible for redelivery
// (citadel-cli issue #871). It is only what a *new* Client is constructed
// with; internal/worker.RedisSource.Connect immediately overrides it per
// instance, via SetStalePendingReclaimMinIdle, with an env-aware value
// derived from the actual resolved long-tier watchdog ceiling
// (internal/worker.ResolveStalePendingReclaimFloor) -- see that function's
// doc comment for the exact formula and why a fixed constant here is not
// enough on its own (citadel-cli#998 review).
//
// This mechanism exists because XREADGROUP's ">" ID NEVER returns an
// already-delivered message again -- not to a replacement consumer after a
// crash, and not even back to the SAME consumer that originally read it
// (verified against miniredis, a Redis-protocol reimplementation: see
// internal/redis's TestXReadGroupNeverRedeliversAlreadyDeliveredMessage).
// Without an explicit reclaim step somewhere in the read path,
// RedisSource.Nack's "don't ACK, let it retry" comment was aspirational:
// the message just sits in the PEL forever, nothing ever reads it again,
// and it never reaches the DLQ either. That turns issue #826's willRetry()
// suppression of the terminal "error" stream event into a PERMANENT
// silence bug (the backend waits forever for a stream event that will
// never arrive) rather than the "deferred until the next attempt or the
// DLQ" behavior it was designed around.
//
// This constant alone is NOT the safety guarantee against stealing a live
// bounded job -- see ResolveStalePendingReclaimFloor for why an operator-
// tunable watchdog timeout requires the floor to be resolved dynamically,
// with a margin, rather than hardcoded here.
//
// A package var (not a const) so tests can shrink it instead of sleeping
// real time.
var StalePendingReclaimMinIdle = 4 * time.Hour

// ReclaimStalePending is ReclaimStalePendingOnQueue for the client's primary
// (single-queue) configuration.
func (c *Client) ReclaimStalePending(ctx context.Context) (*Job, error) {
	return c.ReclaimStalePendingOnQueue(ctx, c.queueName)
}

// ReclaimStalePendingOnQueue attempts to steal exactly one message from
// queue's pending-entries list (PEL) that has been idle at least this
// client's reclaimMinIdle (StalePendingReclaimMinIdle by default, or the
// env-aware value SetStalePendingReclaimMinIdle installed), via XAUTOCLAIM,
// reassigning it to this client's own consumer name. Returns (nil, nil)
// whenever nothing is eligible -- the overwhelming common case on every
// poll -- so callers can unconditionally fall through to their normal read.
//
// XAUTOCLAIM (without JUSTID) increments the message's delivery count on
// every successful claim, including a "self-claim" back to the same
// consumer that already owned it (verified against miniredis: internal/
// redis's TestReclaimStalePendingOnQueue) -- so the existing DLQ cutoff
// (deliveryCount >= MaxAttempts in RedisSource's nextSingle/nextMulti) and
// willRetry's retry signal (internal/worker/runner.go) both continue to work
// completely unmodified. This method only supplies the missing "does
// redelivery actually happen at all" half described above.
//
// Two residual gaps this method does NOT close, both accepted and
// documented rather than fixed here (see CLAUDE.md's Direct-Redis
// redelivery section, and citadel-cli#999 for the proper follow-up fix):
// (1) it can self-steal from -- and therefore double-dispatch -- MULTIPLE
// consumer processes sharing this consumer group (horizontal scaling),
// since a claim only moves PEL ownership in Redis and does nothing to stop
// whichever process is still actually running the handler; (2) a genuinely
// unbounded job (see ResolveStalePendingReclaimFloor) that outlives roughly
// (MaxAttempts-1) reclaim cycles is MoveToDLQ'd and ACKed -- reported dead
// -- even if it is still legitimately executing.
//
// citadel-cli#999 tracks the proper fix: reclaim must not CLAIM an entry it
// shouldn't be claiming, because XAUTOCLAIM increments the delivery count
// as a side effect of the claim call itself, before any Go code sees the
// message -- a caller that detects "already in-flight" and skips executing
// a reclaimed job has already paid that increment, and will still exhaust
// MaxAttempts and still trigger the spurious DLQ move. A naive "skip if
// in-flight" guard on top of this method would NOT close either gap; it
// would only hide gap (2) behind an extra no-op dispatch. The real fix is
// to stop calling a blanket XAUTOCLAIM and instead make a positive
// determination (via XPENDING + targeted XCLAIM, or a true
// consumer-liveness check via XINFO CONSUMERS) that an entry's owning
// consumer is actually gone before ever claiming it.
func (c *Client) ReclaimStalePendingOnQueue(ctx context.Context, queue string) (*Job, error) {
	msgs, _, err := c.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   queue,
		Group:    c.consumerGroup,
		Consumer: c.workerID,
		MinIdle:  c.reclaimMinIdle,
		Start:    "0-0",
		Count:    1,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to reclaim stale pending message on %s: %w", queue, err)
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	return c.parseMessage(msgs[0])
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

	// JQS-Core Section 7.2.1: preserve enqueuedAt in DLQ entry
	if job.EnqueuedAt != "" {
		fields["enqueuedAt"] = job.EnqueuedAt
	} else if ea, ok := job.RawData["enqueuedAt"].(string); ok {
		fields["enqueuedAt"] = ea
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

	// Extract enqueuedAt timestamp (preserved for DLQ entries)
	if enqueuedAt, ok := msg.Values["enqueuedAt"].(string); ok {
		job.EnqueuedAt = enqueuedAt
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

// MoveToDLQ moves a failed message to the Dead Letter Queue.
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

	// JQS-Core Section 7.2.1: preserve enqueuedAt in DLQ entry
	if job.EnqueuedAt != "" {
		fields["enqueuedAt"] = job.EnqueuedAt
	} else if ea, ok := job.RawData["enqueuedAt"].(string); ok {
		fields["enqueuedAt"] = ea
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
func (c *Client) PublishStreamEvent(ctx context.Context, jobID, rayID, eventType string, data map[string]interface{}) error {
	streamName := fmt.Sprintf("stream:v1:%s", jobID)

	// Inject node identity metadata into every event for operator attribution
	if c.nodeMeta != nil {
		if data == nil {
			data = make(map[string]interface{})
		}
		data["meta"] = c.nodeMeta
	}

	event := StreamEvent{
		Version:   "1.0",
		Type:      eventType,
		JobID:     jobID,
		RayID:     rayID,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      data,
	}

	eventJSON, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal stream event: %w", err)
	}

	return c.client.Publish(ctx, streamName, eventJSON).Err()
}

// PublishClaimed publishes a "claimed" event for a job, emitted the moment the
// worker reads it off the queue (before handler execution). The backend
// dispatcher (aceteam#6000) uses it to fast-fail a wedged/dead node that never
// claims, instead of waiting the full result budget.
func (c *Client) PublishClaimed(ctx context.Context, jobID, rayID, agentVersion string) error {
	return c.PublishStreamEvent(ctx, jobID, rayID, "claimed", map[string]interface{}{
		"agent_version": agentVersion,
	})
}

// PublishStart publishes a "start" event for a job.
func (c *Client) PublishStart(ctx context.Context, jobID, rayID, message string) error {
	return c.PublishStreamEvent(ctx, jobID, rayID, "start", map[string]interface{}{
		"message": message,
	})
}

// PublishChunk publishes a "chunk" event for streaming responses.
func (c *Client) PublishChunk(ctx context.Context, jobID, rayID, content string, index int) error {
	return c.PublishStreamEvent(ctx, jobID, rayID, "chunk", map[string]interface{}{
		"content": content,
		"index":   index,
	})
}

// PublishEnd publishes an "end" event when job completes.
func (c *Client) PublishEnd(ctx context.Context, jobID, rayID string, result map[string]interface{}) error {
	return c.PublishStreamEvent(ctx, jobID, rayID, "end", map[string]interface{}{
		"result": result,
	})
}

// PublishError publishes an "error" event when job fails.
func (c *Client) PublishError(ctx context.Context, jobID, rayID, errMsg string, recoverable bool) error {
	return c.PublishStreamEvent(ctx, jobID, rayID, "error", map[string]interface{}{
		"error":       errMsg,
		"recoverable": recoverable,
	})
}

// PublishCancelled publishes a "cancelled" terminal event when a job is cancelled.
func (c *Client) PublishCancelled(ctx context.Context, jobID, rayID, reason string) error {
	return c.PublishStreamEvent(ctx, jobID, rayID, "cancelled", map[string]interface{}{
		"reason": reason,
	})
}

// IsJobCancelled checks whether a cancellation flag exists for the given job.
// The producer sets key "job:cancelled:{jobId}" to signal cancellation.
func (c *Client) IsJobCancelled(ctx context.Context, jobID string) (bool, error) {
	key := fmt.Sprintf("job:cancelled:%s", jobID)
	result, err := c.client.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return result > 0, nil
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

// WatchWake subscribes to a Pub/Sub channel and invokes onWake for every
// message received, until ctx is cancelled (issue #7270). It is the direct-Redis
// half of push-based dispatch: the backend PUBLISHes a nudge on the node's wake
// channel after a targeted XADD, and this fires onWake so the consume loop drains
// its per-node stream immediately instead of waiting out its ~5s poll block.
//
// Best-effort by contract: the ~5s poll remains the correctness backstop, so a
// dropped subscription only costs latency, never a job. The subscribe itself is
// synchronous (so a bad channel surfaces immediately); message delivery then runs
// in a background goroutine that exits on ctx cancellation.
func (c *Client) WatchWake(ctx context.Context, channel string, onWake func()) error {
	pubsub := c.client.Subscribe(ctx, channel)
	// Wait for the subscribe to be confirmed so a failure is returned to the
	// caller rather than swallowed in the goroutine.
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return fmt.Errorf("failed to subscribe to wake channel %s: %w", channel, err)
	}
	ch := pubsub.Channel()
	go func() {
		defer pubsub.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					return
				}
				onWake()
			}
		}
	}()
	return nil
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

// SetNodeMeta sets the node identity metadata that will be included in all stream events.
func (c *Client) SetNodeMeta(nodeID, nodeName string) {
	c.nodeMeta = &NodeMeta{NodeID: nodeID, NodeName: nodeName}
}
