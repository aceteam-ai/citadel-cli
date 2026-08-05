package worker

import (
	"context"
	"fmt"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// APISource implements JobSource using the AceTeam Redis API proxy.
// This is the secure alternative to direct Redis connections.
// Supports consuming from multiple queues by round-robining across them.
type APISource struct {
	client *redisapi.Client
	config APISourceConfig

	// mu guards queueNames, which is read by the run loop (Next) and may be
	// appended to at runtime by AddQueue (e.g. the /agent/resubscribe control
	// endpoint, issue #236) from a different goroutine.
	mu         sync.RWMutex
	queueNames []string // resolved list of queues to consume from
	queueIndex int      // round-robin index for multi-queue polling

	// wake, when set (via EnableWake), makes Next return immediately on a
	// per-node Pub/Sub nudge delivered over the WebSocket, instead of waiting out
	// the HTTP consume block (issue #7270). Nil for the normal poll-only path.
	wake *wakePump
}

// apiWakeDrainBlockMs is the (short) block used for the wake-triggered drain in
// api-proxy mode. The Redis API proxy runs a server-side XREADGROUP BLOCK, so a
// truly non-blocking read is not exposed; a brief block is plenty since the
// nudge is published AFTER the XADD, so the job is already on the stream. Still
// ~10x faster than the 5s poll — the whole point of the wake.
const apiWakeDrainBlockMs = 500

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
//
// This may be retried: the connect-with-backoff loop in cmd/work.go calls it
// repeatedly until a ping succeeds (issue #443). So we must NOT retain a client
// whose ping failed -- otherwise a subsequent call would short-circuit on the
// "already connected" guard and report success without ever verifying the
// endpoint. The client is only assigned to s.client once the ping succeeds, and
// the ping error is wrapped with %w so callers can still type-assert a 429
// (redisapi.RateLimitError) through the chain.
func (s *APISource) Connect(ctx context.Context) error {
	// Skip if already connected and verified.
	if s.client != nil {
		return nil
	}

	client := redisapi.NewClient(redisapi.ClientConfig{
		BaseURL:   s.config.BaseURL,
		Token:     s.config.Token,
		DebugFunc: s.config.DebugFunc,
	})

	// Verify connection before retaining the client.
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("failed to connect to Redis API: %w", err)
	}
	s.client = client

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
	if w := s.getWake(); w != nil {
		return w.next(ctx)
	}
	return s.readOnce(ctx, s.config.BlockMs)
}

// getWake reads the wake pump under mu. EnableWake may be called from a
// different goroutine than the run loop (e.g. /agent/resubscribe), so the
// pointer is guarded like queueNames rather than read racily.
func (s *APISource) getWake() *wakePump {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.wake
}

// readOnce performs one consume of the subscribed queue(s) with the given block
// timeout. Core shared by the poll-only path and the wakePump closures.
func (s *APISource) readOnce(ctx context.Context, blockMs int) (*Job, error) {
	queues := s.snapshotQueues()
	if len(queues) == 1 {
		return s.nextSingle(ctx, queues[0], blockMs)
	}
	return s.nextMulti(ctx, queues, blockMs)
}

// EnableWake subscribes to the node's Pub/Sub wake channel over the WebSocket and
// routes Next through the wakePump so a targeted-dispatch nudge is consumed
// immediately (issue #7270). Requires the WebSocket to be enabled
// (Client.EnableWebSocket, done in cmd/work.go); if it is not connected the wake
// cannot be delivered and the node stays correctly poll-only. Best-effort: a
// subscribe failure returns the error and leaves the source poll-only.
func (s *APISource) EnableWake(ctx context.Context, channel string) error {
	if channel == "" || s.client == nil || s.getWake() != nil {
		return nil
	}
	ws := s.client.WebSocket()
	if ws == nil || !ws.IsConnected() {
		return fmt.Errorf("websocket not connected; wake unavailable (staying poll-only)")
	}
	pump := newWakePump(
		func(c context.Context) (*Job, error) { return s.readOnce(c, s.config.BlockMs) },
		func(c context.Context) (*Job, error) { return s.readOnce(c, apiWakeDrainBlockMs) },
	)
	// Incoming Pub/Sub messages arrive as WSMessage{Type:"message", Channel:...}.
	// Filter to our wake channel and coalesce into the pump. NOTE: the WSClient
	// keeps a single handler per message type; nothing else registers "message"
	// in `citadel work` (the chat REPL is not co-resident with the worker), so
	// this slot is free. If that ever changes, WSClient needs multi-handler fan-out.
	ws.OnMessage("message", func(msg redisapi.WSMessage) {
		if msg.Channel == channel {
			pump.signal()
		}
	})
	if err := ws.Subscribe(ctx, channel); err != nil {
		return fmt.Errorf("failed to subscribe to wake channel %s: %w", channel, err)
	}
	// Re-subscribe after a WS reconnect so the wake survives connection churn.
	ws.OnReconnect(func() {
		if err := ws.Subscribe(context.Background(), channel); err != nil {
			s.log("warning", "wake re-subscribe after reconnect failed: %v", err)
		}
	})
	pump.start(ctx)
	s.mu.Lock()
	if s.wake != nil { // lost a race with a concurrent EnableWake; keep the winner
		s.mu.Unlock()
		return nil
	}
	s.wake = pump
	s.mu.Unlock()
	s.log("info", "   - Push-wake enabled: %s", channel)
	return nil
}

// snapshotQueues returns a stable copy of the queue list for one poll cycle,
// so concurrent AddQueue calls (e.g. /agent/resubscribe) don't race the loop.
func (s *APISource) snapshotQueues() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.queueNames...)
}

// nextSingle reads from a single queue (original behavior).
func (s *APISource) nextSingle(ctx context.Context, queue string, blockMs int) (*Job, error) {
	apiJob, err := s.client.ConsumeJob(ctx, redisapi.ConsumeRequest{
		Queue:    queue,
		Group:    s.config.ConsumerGroup,
		Consumer: s.client.WorkerID(),
		Count:    1,
		BlockMs:  blockMs,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to consume job from API: %w", err)
	}

	if apiJob == nil {
		return nil, nil
	}

	job := s.convertJob(apiJob)
	job.SourceQueue = queue
	return job, nil
}

// nextMulti round-robins across queues with a shorter block timeout.
// Each poll checks one queue; if empty, advances to the next.
// Individual queue failures (e.g., rejected by server validation) are
// logged and skipped rather than failing the entire poll cycle. Only
// when all queues error does the caller see an error (triggering backoff).
func (s *APISource) nextMulti(ctx context.Context, queues []string, blockMs int) (*Job, error) {
	// Use a shorter block per queue so we cycle through them all within
	// roughly the configured block timeout.
	perQueueBlockMs := blockMs / len(queues)
	if perQueueBlockMs < 500 {
		perQueueBlockMs = 500
	}

	var lastErr error
	var lastQueue string
	errCount := 0

	for i := 0; i < len(queues); i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		queue := queues[s.queueIndex%len(queues)]
		s.queueIndex = (s.queueIndex + 1) % len(queues)

		apiJob, err := s.client.ConsumeJob(ctx, redisapi.ConsumeRequest{
			Queue:    queue,
			Group:    s.config.ConsumerGroup,
			Consumer: s.client.WorkerID(),
			Count:    1,
			BlockMs:  perQueueBlockMs,
		})
		if err != nil {
			// Skip this queue so one rejected queue can't block the others.
			// We deliberately do NOT log per-queue here: a transient consume
			// failure is self-healing and logging it every poll floods the
			// activity panel. The cycle-level outcome is coalesced by the
			// runner instead (the queue name is carried in the wrapped error).
			lastErr = err
			lastQueue = queue
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
	if errCount == len(queues) {
		return nil, fmt.Errorf("all %d queues failed (last: %s): %w", len(queues), lastQueue, lastErr)
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
		if qs := s.snapshotQueues(); len(qs) > 0 {
			queue = qs[0]
		}
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

// Fail is a terminal failure: record "failed" status (with structured data) and
// ACK the message so it is removed from the consumer group's PEL. Used for
// failures that will never succeed on retry (e.g. an unsupported job type).
func (s *APISource) Fail(ctx context.Context, job *Job, err error, data map[string]any) error {
	status := map[string]any{"error": err.Error()}
	for k, v := range data {
		status[k] = v
	}
	s.client.SetJobStatus(ctx, job.ID, "failed", status)
	queue := job.SourceQueue
	if queue == "" {
		if qs := s.snapshotQueues(); len(qs) > 0 {
			queue = qs[0]
		}
	}
	return s.client.AcknowledgeJob(ctx, redisapi.AcknowledgeRequest{
		Queue:     queue,
		Group:     s.config.ConsumerGroup,
		MessageID: job.MessageID,
	})
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

// LastConsumeStatus returns the HTTP status code of the most recent consume
// request, or 0 if not yet connected/polled. Satisfies consumeStatusReporter
// so the runner can surface it for worker introspection (issue #236).
func (s *APISource) LastConsumeStatus() int {
	if s.client == nil {
		return 0
	}
	return s.client.LastConsumeStatus()
}

// QueueNames returns the list of queues being consumed.
func (s *APISource) QueueNames() []string {
	return s.snapshotQueues()
}

// AddQueue appends an additional queue to consume from after construction.
//
// This is used to subscribe to the worker's per-node shell stream once the
// node's Headscale ID is known (issue #3914), which happens after the source
// is built. The Redis API proxy creates the consumer group lazily on the first
// XREADGROUP, so no explicit group-creation call is needed here. A blank or
// already-present queue is ignored. Guarded by mu so it is safe to call at
// runtime (e.g. from the /agent/resubscribe control endpoint, issue #236)
// concurrently with the run loop, which reads via snapshotQueues.
func (s *APISource) AddQueue(queue string) {
	if queue == "" {
		return
	}
	s.mu.Lock()
	for _, q := range s.queueNames {
		if q == queue {
			s.mu.Unlock()
			return
		}
	}
	s.queueNames = append(s.queueNames, queue)
	s.mu.Unlock()
	s.log("info", "   - Added queue: %s", queue)
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
