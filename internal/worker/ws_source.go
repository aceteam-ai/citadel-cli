package worker

import (
	"context"
	"fmt"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// WSSource implements JobSource using the existing WebSocket connection for
// server-pushed job delivery. Instead of HTTP polling, the server runs a
// persistent XREADGROUP BLOCK loop and pushes "job" messages to the client.
type WSSource struct {
	client *redisapi.Client
	config WSSourceConfig

	// mu guards queueNames which may be mutated by AddQueue at runtime.
	mu         sync.RWMutex
	queueNames []string

	// jobs is the channel that receives parsed jobs from the WebSocket handler.
	// Buffered to avoid blocking the WSClient readLoop.
	jobs chan *Job

	// done is closed by Close to signal the handler and Next to stop.
	done     chan struct{}
	doneOnce sync.Once
}

// WSSourceConfig holds configuration for WSSource.
type WSSourceConfig struct {
	// Client is the redisapi.Client with WebSocket already enabled (or to be enabled).
	Client *redisapi.Client

	// QueueName is the Redis Stream to consume from (single queue, backwards compat).
	QueueName string

	// QueueNames is the list of Redis Streams to consume from (multi-queue mode).
	// If set, QueueName is ignored.
	QueueNames []string

	// ConsumerGroup is the consumer group name (default: "citadel-workers").
	ConsumerGroup string

	// BlockMs is the server-side XREADGROUP BLOCK timeout (default: 5000).
	BlockMs int

	// DebugFunc is an optional callback for debug logging.
	DebugFunc func(format string, args ...any)

	// LogFn is an optional callback for logging (if nil, prints to stdout).
	LogFn func(level, msg string)
}

// NewWSSource creates a new WebSocket-backed job source.
func NewWSSource(cfg WSSourceConfig) *WSSource {
	if cfg.ConsumerGroup == "" {
		cfg.ConsumerGroup = "citadel-workers"
	}
	if cfg.BlockMs == 0 {
		cfg.BlockMs = 5000
	}

	var queues []string
	if len(cfg.QueueNames) > 0 {
		queues = cfg.QueueNames
	} else if cfg.QueueName != "" {
		queues = []string{cfg.QueueName}
	} else {
		queues = []string{"jobs:v1:cpu-general"}
	}

	return &WSSource{
		client:     cfg.Client,
		config:     cfg,
		queueNames: queues,
		jobs:       make(chan *Job, 16),
		done:       make(chan struct{}),
	}
}

// Name returns the source identifier.
func (s *WSSource) Name() string {
	return "websocket"
}

// log outputs a message.
func (s *WSSource) log(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if s.config.LogFn != nil {
		s.config.LogFn(level, msg)
	} else {
		fmt.Printf("%s\n", msg)
	}
}

// debug logs via the debug callback if configured.
func (s *WSSource) debug(format string, args ...any) {
	if s.config.DebugFunc != nil {
		s.config.DebugFunc(format, args...)
	}
}

// Connect enables the WebSocket on the client, registers the job handler,
// sets up the reconnect callback, and sends the initial consume message.
func (s *WSSource) Connect(ctx context.Context) error {
	if s.client == nil {
		return fmt.Errorf("WSSource requires a non-nil redisapi.Client")
	}

	// Enable WebSocket if not already connected
	if err := s.client.EnableWebSocket(ctx); err != nil {
		return fmt.Errorf("failed to enable WebSocket: %w", err)
	}

	ws := s.client.WebSocket()
	if ws == nil {
		return fmt.Errorf("WebSocket client is nil after EnableWebSocket")
	}

	// Register handler for "job" messages. The server pushes these
	// continuously from its XREADGROUP BLOCK loop.
	ws.OnMessage("job", func(msg redisapi.WSMessage) {
		job, err := s.convertWSJob(msg)
		if err != nil {
			s.debug("ws_source: failed to convert job message: %v", err)
			return
		}

		// Blocking send with cancel: applies backpressure if the buffer
		// is full (runner is busy). The <-s.done case unblocks a stuck
		// readLoop on Close so shutdown is never wedged.
		select {
		case s.jobs <- job:
		case <-s.done:
		}
	})

	// Register handler for server-side errors (e.g. rejected queue name,
	// missing device_redis:read scope). Without this the client would
	// silently believe it is consuming while receiving nothing — the
	// exact failure mode #236/#3924 eliminated on the HTTP path.
	ws.OnMessage("error", func(msg redisapi.WSMessage) {
		s.log("error", "WebSocket server error: %s", msg.Error)
	})

	// Register handler for "consuming" confirmation from the server.
	ws.OnMessage("consuming", func(msg redisapi.WSMessage) {
		s.debug("ws_source: server confirmed consuming queues=%v", msg.Queues)
	})

	// Register handler for "acked" confirmation from the server.
	ws.OnMessage("acked", func(msg redisapi.WSMessage) {
		s.debug("ws_source: server confirmed ack messageId=%s", msg.MessageID)
	})

	// Register reconnect callback to re-send consume with current queue list.
	ws.OnReconnect(func() {
		s.log("info", "WebSocket reconnected, re-sending consume")
		if err := s.sendConsume(); err != nil {
			s.log("warning", "Failed to re-send consume after reconnect: %v", err)
		}
	})

	// Verify connection with a ping
	if err := s.client.Ping(ctx); err != nil {
		return fmt.Errorf("failed to ping API: %w", err)
	}

	// Send the initial consume message
	if err := s.sendConsume(); err != nil {
		return fmt.Errorf("failed to start consuming: %w", err)
	}

	queues := s.snapshotQueues()
	s.log("info", "   - Source: websocket")
	s.log("info", "   - API: %s", s.client.BaseURL())
	s.log("info", "   - Worker ID: %s", s.client.WorkerID())
	if len(queues) == 1 {
		s.log("info", "   - Queue: %s", queues[0])
	} else {
		s.log("info", "   - Queues (%d):", len(queues))
		for _, q := range queues {
			s.log("info", "     - %s", q)
		}
	}
	s.log("info", "   - Consumer group: %s", s.config.ConsumerGroup)

	return nil
}

// sendConsume sends the consume message with the current queue list.
func (s *WSSource) sendConsume() error {
	if s.client == nil {
		return fmt.Errorf("client not initialized")
	}
	ws := s.client.WebSocket()
	if ws == nil {
		return fmt.Errorf("WebSocket not available")
	}

	queues := s.snapshotQueues()
	return ws.StartConsume(queues, s.config.ConsumerGroup, s.client.WorkerID(), 1, s.config.BlockMs)
}

// Next blocks until a job is available or the context is cancelled.
func (s *WSSource) Next(ctx context.Context) (*Job, error) {
	select {
	case job := <-s.jobs:
		return job, nil
	case <-s.done:
		return nil, fmt.Errorf("WSSource closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Ack acknowledges successful job completion. Sends an ack over WebSocket
// and also updates job status via HTTP (same as APISource).
func (s *WSSource) Ack(ctx context.Context, job *Job) error {
	// Update job status via HTTP
	s.client.SetJobStatus(ctx, job.ID, "completed", nil)

	queue := job.SourceQueue
	if queue == "" {
		if qs := s.snapshotQueues(); len(qs) > 0 {
			queue = qs[0]
		}
	}

	// Ack via WebSocket
	ws := s.client.WebSocket()
	if ws != nil && ws.IsConnected() {
		if err := ws.AckJob(queue, s.config.ConsumerGroup, job.MessageID); err != nil {
			s.debug("ws_source: WS ack failed, falling back to HTTP: %v", err)
			return s.client.AcknowledgeJob(ctx, redisapi.AcknowledgeRequest{
				Queue:     queue,
				Group:     s.config.ConsumerGroup,
				MessageID: job.MessageID,
			})
		}
		return nil
	}

	// Fallback to HTTP ack if WebSocket is disconnected
	return s.client.AcknowledgeJob(ctx, redisapi.AcknowledgeRequest{
		Queue:     queue,
		Group:     s.config.ConsumerGroup,
		MessageID: job.MessageID,
	})
}

// Nack indicates job failure. Updates status but does NOT ack, allowing retry.
func (s *WSSource) Nack(ctx context.Context, job *Job, err error) error {
	s.client.SetJobStatus(ctx, job.ID, "failed", map[string]any{
		"error": err.Error(),
	})
	return nil
}

// Fail is a terminal failure: record "failed" status (with structured data) and
// ACK the message so it is removed from the consumer group's PEL. Used for
// failures that will never succeed on retry (e.g. an unsupported job type). The
// ack mirrors Ack: over WebSocket when connected, falling back to HTTP.
func (s *WSSource) Fail(ctx context.Context, job *Job, err error, data map[string]any) error {
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

	ws := s.client.WebSocket()
	if ws != nil && ws.IsConnected() {
		if ackErr := ws.AckJob(queue, s.config.ConsumerGroup, job.MessageID); ackErr != nil {
			s.debug("ws_source: WS ack failed on Fail, falling back to HTTP: %v", ackErr)
			return s.client.AcknowledgeJob(ctx, redisapi.AcknowledgeRequest{
				Queue:     queue,
				Group:     s.config.ConsumerGroup,
				MessageID: job.MessageID,
			})
		}
		return nil
	}

	return s.client.AcknowledgeJob(ctx, redisapi.AcknowledgeRequest{
		Queue:     queue,
		Group:     s.config.ConsumerGroup,
		MessageID: job.MessageID,
	})
}

// IsJobCancelled checks whether a job has been cancelled by the producer.
func (s *WSSource) IsJobCancelled(ctx context.Context, jobID string) bool {
	cancelled, err := s.client.IsJobCancelled(ctx, jobID)
	if err != nil {
		s.log("warning", "Failed to check cancellation for %s: %v", jobID, err)
		return false
	}
	return cancelled
}

// Close stops consuming and closes the jobs channel.
func (s *WSSource) Close() error {
	s.doneOnce.Do(func() {
		close(s.done)
	})

	// Send stop_consume to halt the server-side loop
	ws := s.client.WebSocket()
	if ws != nil && ws.IsConnected() {
		if err := ws.StopConsume(); err != nil {
			s.debug("ws_source: failed to send stop_consume: %v", err)
		}
	}

	if s.client != nil {
		return s.client.Close()
	}
	return nil
}

// Client returns the underlying API client for stream writing.
func (s *WSSource) Client() *redisapi.Client {
	return s.client
}

// QueueNames returns the list of queues being consumed.
func (s *WSSource) QueueNames() []string {
	return s.snapshotQueues()
}

// AddQueue appends an additional queue to consume from after construction.
// Thread-safe. After adding the queue, re-sends the consume message with the
// updated queue list (the server stops the old loop and starts a new one).
func (s *WSSource) AddQueue(queue string) {
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

	// Re-send consume with updated queue list
	if err := s.sendConsume(); err != nil {
		s.log("warning", "Failed to re-send consume after AddQueue: %v", err)
	}
}

// snapshotQueues returns a stable copy of the queue list.
func (s *WSSource) snapshotQueues() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.queueNames...)
}

// convertWSJob converts a WebSocket "job" message to a worker.Job.
// The server sends: { type: "job", queue: "...", id: "1234567890-0", data: { jobId, type, payload, enqueuedAt } }
// The data fields mirror StreamMessageData, so we reuse ParseStreamMessage.
func (s *WSSource) convertWSJob(msg redisapi.WSMessage) (*Job, error) {
	if msg.Data == nil {
		return nil, fmt.Errorf("job message has nil data")
	}

	// Build a StreamMessage from the WS fields to reuse ParseStreamMessage
	streamMsg := redisapi.StreamMessage{
		ID: msg.ID,
		Data: redisapi.StreamMessageData{
			JobID:      msg.Data["jobId"],
			Type:       msg.Data["type"],
			Payload:    msg.Data["payload"],
			EnqueuedAt: msg.Data["enqueuedAt"],
			RayID:      msg.Data["rayId"],
		},
	}

	apiJob, err := redisapi.ParseStreamMessage(streamMsg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse stream message: %w", err)
	}

	job := &Job{
		ID:          apiJob.JobID,
		Type:        apiJob.Type,
		Payload:     apiJob.Payload,
		Source:      "websocket",
		MessageID:   apiJob.MessageID,
		SourceQueue: msg.Queue,
	}

	// Extract rayId
	if apiJob.RawData != nil {
		if rayID, ok := apiJob.RawData["rayId"].(string); ok && rayID != "" {
			job.RayID = rayID
		}
	}
	if job.RayID == "" && apiJob.Payload != nil {
		if rayID, ok := apiJob.Payload["rayId"].(string); ok {
			job.RayID = rayID
		}
	}

	return job, nil
}

// Ensure WSSource implements JobSource at compile time.
var _ JobSource = (*WSSource)(nil)
