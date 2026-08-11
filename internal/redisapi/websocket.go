package redisapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSClient provides WebSocket-based access to the AceTeam Redis API.
// Used for real-time pub/sub operations instead of HTTP polling.
type WSClient struct {
	baseURL   string
	token     string
	conn      *websocket.Conn
	connMu    sync.RWMutex
	connected bool

	// writeMu serializes gorilla "write methods" (WriteMessage,
	// SetWriteDeadline) on conn. gorilla supports exactly ONE concurrent
	// writer and panics with "concurrent write to websocket connection"
	// otherwise, which kills the whole worker process (#720).
	//
	// It is deliberately NOT connMu: connMu guards the conn POINTER, is taken
	// shared (RLock) by every reader, and is released before any write
	// happens, so it serializes nothing on the wire. Keeping them separate
	// also keeps pointer reads cheap while a slow write is in flight.
	//
	// Lock order: never acquire connMu while holding writeMu. sendMessage
	// releases connMu before taking writeMu; Close takes connMu then writeMu.
	writeMu sync.Mutex

	// writeTimeout bounds a single WriteMessage. Serializing writes means a
	// blocked write no longer stalls just its own caller, it stalls every
	// publisher queued behind writeMu, so the write has to be bounded.
	// Overridden in tests; see defaultWriteTimeout for the value rationale.
	writeTimeout time.Duration

	// Message handlers
	handlers   map[string]func(WSMessage)
	handlersMu sync.RWMutex

	// Subscriptions to restore on reconnect
	subscriptions   map[string]bool
	subscriptionsMu sync.RWMutex

	// Control channels
	done     chan struct{}
	stopOnce sync.Once

	// Reconnection settings.
	//
	// reconnectBackoff is guarded by connMu (#728). It is written by
	// connectLocked, whose caller already holds connMu for write, and it is
	// advanced by nextReconnectDelay, which takes connMu for the whole
	// read-modify-write and releases it before the caller sleeps. Nothing may
	// touch it unlocked: reconnect used to, and a torn or stale read there
	// shortens a backoff.
	//
	// reconnectEnabled and maxBackoff are set once in NewWSClient and never
	// written again, so they need no guard.
	reconnectEnabled bool
	reconnectBackoff time.Duration
	maxBackoff       time.Duration

	// Reconnect callbacks fired after a successful reconnection
	reconnectCallbacks   []func()
	reconnectCallbacksMu sync.RWMutex

	// Debug callback
	debugFunc func(format string, args ...any)
}

// WSClientConfig holds configuration for the WebSocket client.
type WSClientConfig struct {
	// BaseURL is the AceTeam API base URL (e.g., "https://aceteam.ai")
	BaseURL string

	// Token is the device_api_token from device authentication
	Token string

	// ReconnectEnabled enables automatic reconnection (default: true)
	ReconnectEnabled bool

	// DebugFunc is an optional callback for debug logging
	DebugFunc func(format string, args ...any)
}

// WSMessage represents a message received from or sent to the WebSocket.
type WSMessage struct {
	Type     string         `json:"type"`
	Channel  string         `json:"channel,omitempty"`
	Channels []string       `json:"channels,omitempty"`
	Message  map[string]any `json:"message,omitempty"`
	Error    string         `json:"error,omitempty"`

	// Consume-related fields (job delivery protocol)
	Queue     string            `json:"queue,omitempty"`
	Queues    []string          `json:"queues,omitempty"`
	Group     string            `json:"group,omitempty"`
	Consumer  string            `json:"consumer,omitempty"`
	Count     int               `json:"count,omitempty"`
	BlockMs   int               `json:"blockMs,omitempty"`
	ID        string            `json:"id,omitempty"`        // Stream message ID (job delivery)
	MessageID string            `json:"messageId,omitempty"` // Ack message ID
	Data      map[string]string `json:"data,omitempty"`      // Job data fields from stream
}

// initialReconnectBackoff is the delay before the first reconnect attempt, and
// the value the schedule returns to after any successful connect. It lives in
// one place so the constructor and the reset in connectLocked cannot drift.
const initialReconnectBackoff = time.Second

// NewWSClient creates a new WebSocket client.
func NewWSClient(cfg WSClientConfig) *WSClient {
	reconnectEnabled := cfg.ReconnectEnabled
	// Default to true if not explicitly set
	if cfg.BaseURL != "" && !cfg.ReconnectEnabled {
		reconnectEnabled = true
	}

	return &WSClient{
		baseURL:          cfg.BaseURL,
		token:            cfg.Token,
		handlers:         make(map[string]func(WSMessage)),
		subscriptions:    make(map[string]bool),
		done:             make(chan struct{}),
		reconnectEnabled: reconnectEnabled,
		reconnectBackoff: initialReconnectBackoff,
		maxBackoff:       time.Minute,
		writeTimeout:     defaultWriteTimeout,
		debugFunc:        cfg.DebugFunc,
	}
}

// debug logs a message if debug function is configured
func (c *WSClient) debug(format string, args ...any) {
	if c.debugFunc != nil {
		c.debugFunc(format, args...)
	}
}

// Connect establishes the WebSocket connection.
func (c *WSClient) Connect(ctx context.Context) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.connected {
		return nil
	}

	if err := c.connectLocked(ctx); err != nil {
		return err
	}

	// Start read loop
	go c.readLoop()

	return nil
}

// connectLocked establishes connection (caller must hold connMu lock)
func (c *WSClient) connectLocked(ctx context.Context) error {
	// Convert HTTP URL to WebSocket URL
	wsURL, err := c.getWSURL()
	if err != nil {
		return fmt.Errorf("failed to build WebSocket URL: %w", err)
	}

	c.debug("ws: connecting to %s", wsURL)

	// Set up headers with auth token
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.token)

	// Connect with context
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		if resp != nil {
			// A rejected upgrade carries the same rate-limit hint the REST
			// routes do. Return it TYPED so callers that retry (issue #723) can
			// honor retry_after instead of falling into a generic 2s..2min
			// backoff and polling tighter than the server asked -- the tight
			// loop that burned the daily quota in #443.
			if resp.StatusCode == http.StatusTooManyRequests {
				return fmt.Errorf("WebSocket connection failed: %w", rateLimitFromUpgrade(resp))
			}
			return fmt.Errorf("WebSocket connection failed with status %d: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("WebSocket connection failed: %w", err)
	}

	c.conn = conn
	c.connected = true
	// Reset the backoff schedule on a successful connection. The caller holds
	// connMu for write, which is what makes this assignment safe against
	// nextReconnectDelay running in a reconnect goroutine (#728).
	c.reconnectBackoff = initialReconnectBackoff

	c.debug("ws: connected successfully")

	// Restore subscriptions if any
	c.subscriptionsMu.RLock()
	channels := make([]string, 0, len(c.subscriptions))
	for ch := range c.subscriptions {
		channels = append(channels, ch)
	}
	c.subscriptionsMu.RUnlock()

	if len(channels) > 0 {
		c.debug("ws: restoring %d subscriptions", len(channels))
		// Send subscribe without holding locks
		go func() {
			if err := c.Subscribe(context.Background(), channels...); err != nil {
				c.debug("ws: failed to restore subscriptions: %v", err)
			}
		}()
	}

	return nil
}

// getWSURL converts the HTTP base URL to a WebSocket URL
func (c *WSClient) getWSURL() (string, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", err
	}

	// Convert scheme
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}

	// Add WebSocket path
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/fabric/redis/ws"

	return u.String(), nil
}

// readLoop continuously reads messages from the WebSocket
func (c *WSClient) readLoop() {
	for {
		select {
		case <-c.done:
			return
		default:
		}

		c.connMu.RLock()
		conn := c.conn
		connected := c.connected
		c.connMu.RUnlock()

		if !connected || conn == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			c.debug("ws: read error: %v", err)
			c.handleDisconnect(conn)
			continue
		}

		var msg WSMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			c.debug("ws: failed to parse message: %v", err)
			continue
		}

		c.debug("ws: received message type=%s", msg.Type)

		// Handle message based on type
		c.handlersMu.RLock()
		handler, ok := c.handlers[msg.Type]
		c.handlersMu.RUnlock()

		if ok {
			handler(msg)
		}

		// Also call wildcard handler if set
		c.handlersMu.RLock()
		wildcardHandler, ok := c.handlers["*"]
		c.handlersMu.RUnlock()

		if ok {
			wildcardHandler(msg)
		}
	}
}

// handleDisconnect tears down conn and schedules a reconnect.
//
// conn is the connection the caller observed failing. If the client has already
// moved on to a different connection the call is a no-op, so a slow failure
// cannot close a healthy socket that reconnect established in the meantime.
func (c *WSClient) handleDisconnect(conn *websocket.Conn) {
	c.connMu.Lock()
	if conn != nil && c.conn != conn {
		// Already replaced; whoever swapped it owns the old connection.
		c.connMu.Unlock()
		return
	}
	c.connected = false
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.connMu.Unlock()

	if !c.reconnectEnabled {
		return
	}

	// Attempt reconnection with backoff
	go c.reconnect()
}

// nextReconnectDelay returns how long the caller should wait before its next
// reconnect attempt, and advances the schedule for the attempt after that.
//
// The read, the doubling and the clamp all happen in ONE critical section under
// connMu, and the caller sleeps on the returned value instead of re-reading the
// field. reconnect used to do all of it with no lock at all, racing the reset in
// connectLocked, which runs under connMu (#728). It also read the field twice,
// once to log the delay and once to sleep it, so a reset landing between the two
// made the node retry sooner than it had just announced.
//
// Both defects point the same way, and it is the way that hurts: anything that
// silently shortens a backoff moves toward the #443 restart storm referenced
// above connectWithBackoff in cmd/work.go, where tight retries burned the node's
// daily Redis-API quota and locked it out for about a day.
//
// connMu is reused deliberately rather than adding a third mutex to this file.
// The reset has to live in connectLocked, which is the only place that knows a
// dial succeeded and whose caller already holds connMu, so connMu costs no extra
// lock and no nested acquire. connMu is NOT held across the sleep.
func (c *WSClient) nextReconnectDelay() time.Duration {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	delay := c.reconnectBackoff

	c.reconnectBackoff *= 2
	if c.reconnectBackoff > c.maxBackoff {
		c.reconnectBackoff = c.maxBackoff
	}

	return delay
}

// reconnect attempts to reconnect with exponential backoff
func (c *WSClient) reconnect() {
	for {
		select {
		case <-c.done:
			return
		default:
		}

		c.connMu.RLock()
		alreadyConnected := c.connected
		c.connMu.RUnlock()

		if alreadyConnected {
			return
		}

		delay := c.nextReconnectDelay()
		c.debug("ws: attempting reconnect in %v", delay)
		time.Sleep(delay)

		c.connMu.Lock()
		err := c.connectLocked(context.Background())
		c.connMu.Unlock()

		if err != nil {
			c.debug("ws: reconnect failed: %v", err)
			continue
		}

		c.debug("ws: reconnected successfully")

		// Fire reconnect callbacks AFTER releasing connMu to avoid
		// deadlock (callbacks may call sendMessage which takes RLock).
		c.reconnectCallbacksMu.RLock()
		cbs := make([]func(), len(c.reconnectCallbacks))
		copy(cbs, c.reconnectCallbacks)
		c.reconnectCallbacksMu.RUnlock()

		for _, cb := range cbs {
			cb()
		}

		return
	}
}

// Subscribe subscribes to one or more channels.
func (c *WSClient) Subscribe(ctx context.Context, channels ...string) error {
	if len(channels) == 0 {
		return nil
	}

	// Track subscriptions for reconnect
	c.subscriptionsMu.Lock()
	for _, ch := range channels {
		c.subscriptions[ch] = true
	}
	c.subscriptionsMu.Unlock()

	msg := WSMessage{
		Type:     "subscribe",
		Channels: channels,
	}

	return c.sendMessage(ctx, msg)
}

// Unsubscribe unsubscribes from one or more channels.
func (c *WSClient) Unsubscribe(ctx context.Context, channels ...string) error {
	if len(channels) == 0 {
		return nil
	}

	// Remove from tracked subscriptions
	c.subscriptionsMu.Lock()
	for _, ch := range channels {
		delete(c.subscriptions, ch)
	}
	c.subscriptionsMu.Unlock()

	msg := WSMessage{
		Type:     "unsubscribe",
		Channels: channels,
	}

	return c.sendMessage(ctx, msg)
}

// Publish publishes a message to a channel.
func (c *WSClient) Publish(ctx context.Context, channel string, message any) error {
	// Convert message to map if needed
	var msgMap map[string]any
	switch m := message.(type) {
	case map[string]any:
		msgMap = m
	default:
		// Marshal and unmarshal to convert struct to map
		data, err := json.Marshal(message)
		if err != nil {
			return fmt.Errorf("failed to marshal message: %w", err)
		}
		if err := json.Unmarshal(data, &msgMap); err != nil {
			return fmt.Errorf("failed to convert message to map: %w", err)
		}
	}

	msg := WSMessage{
		Type:    "publish",
		Channel: channel,
		Message: msgMap,
	}

	return c.sendMessage(ctx, msg)
}

// sendMessage sends a message over the WebSocket
func (c *WSClient) sendMessage(_ context.Context, msg WSMessage) error {
	c.connMu.RLock()
	conn := c.conn
	connected := c.connected
	c.connMu.RUnlock()

	if !connected || conn == nil {
		return fmt.Errorf("WebSocket not connected")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	c.debug("ws: sending message type=%s", msg.Type)

	// Single-writer discipline (#720). Callers are genuinely concurrent: the
	// heartbeat publisher ticks every 30s, job streaming publishes per chunk,
	// the consume loop acks per job, and reconnect re-subscribes from its own
	// goroutine. Without this lock they all enter WriteMessage at once.
	//
	// The deadline is set AND cleared inside the same critical section:
	// SetWriteDeadline is one of gorilla's write methods (a bare field
	// assignment on the Conn), so touching it outside writeMu would race an
	// in-flight WriteMessage, which is the bug this fix found in Close.
	c.writeMu.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	err = conn.WriteMessage(websocket.TextMessage, data)
	_ = conn.SetWriteDeadline(time.Time{})
	c.writeMu.Unlock()

	if err != nil {
		// gorilla latches the first write error (Conn.writeErr, set once by
		// writeFatal) and returns it from every later write, so this
		// connection can never carry another byte. Tear it down: that flips
		// IsConnected to false so Client.Publish falls back to HTTP, and it
		// kicks reconnect. Without it the socket stays "connected" while
		// every publish fails forever, which is the silent stall shape this
		// whole fix exists to avoid.
		c.handleDisconnect(conn)
		return fmt.Errorf("failed to send message: %w", err)
	}

	return nil
}

// closeWriteWait bounds both the close-handshake write and how long Close
// waits for an in-flight write to finish before giving up on the handshake.
const closeWriteWait = 2 * time.Second

// defaultWriteTimeout bounds a single WriteMessage.
//
// 15s is chosen against the slowest-cadence caller rather than against network
// latency: the heartbeat publishes every 30s, so a 15s bound guarantees a
// wedged socket surfaces as an error before the next tick can queue behind it,
// and at most one heartbeat interval is ever affected. It is also far above any
// plausible time to hand a few-KB frame to the kernel on a healthy connection,
// so it should never fire in normal operation, and well under the worker
// self-heal stall timer (600s) that would otherwise be the only backstop.
const defaultWriteTimeout = 15 * time.Second

// tryLockWrite acquires writeMu, giving up after d. Close must not block
// forever behind a wedged writer (see the deadline note in Close), so it needs
// a bounded acquire rather than a plain Lock.
func (c *WSClient) tryLockWrite(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if c.writeMu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// OnMessage registers a handler for a specific message type.
// Use "*" to handle all message types.
func (c *WSClient) OnMessage(msgType string, handler func(WSMessage)) {
	c.handlersMu.Lock()
	c.handlers[msgType] = handler
	c.handlersMu.Unlock()
}

// OnReconnect registers a callback that fires after a successful reconnection.
// The callback is NOT called on the initial Connect -- only on reconnects.
// Callbacks run outside of the connection lock so it is safe to call sendMessage.
func (c *WSClient) OnReconnect(callback func()) {
	c.reconnectCallbacksMu.Lock()
	c.reconnectCallbacks = append(c.reconnectCallbacks, callback)
	c.reconnectCallbacksMu.Unlock()
}

// StartConsume sends a consume message to begin receiving jobs from the server.
// The server will start a persistent XREADGROUP BLOCK loop and push job messages.
func (c *WSClient) StartConsume(queues []string, group, consumer string, count, blockMs int) error {
	msg := WSMessage{
		Type:     "consume",
		Queues:   queues,
		Group:    group,
		Consumer: consumer,
		Count:    count,
		BlockMs:  blockMs,
	}
	return c.sendMessage(context.Background(), msg)
}

// StopConsume sends a stop_consume message to halt the server-side consume loop.
func (c *WSClient) StopConsume() error {
	msg := WSMessage{
		Type: "stop_consume",
	}
	return c.sendMessage(context.Background(), msg)
}

// AckJob sends an ack message to acknowledge a processed job.
func (c *WSClient) AckJob(queue, group, messageID string) error {
	msg := WSMessage{
		Type:      "ack",
		Queue:     queue,
		Group:     group,
		MessageID: messageID,
	}
	return c.sendMessage(context.Background(), msg)
}

// IsConnected returns whether the WebSocket is currently connected.
func (c *WSClient) IsConnected() bool {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.connected
}

// Close closes the WebSocket connection.
func (c *WSClient) Close() error {
	c.stopOnce.Do(func() {
		close(c.done)
	})

	c.connMu.Lock()
	defer c.connMu.Unlock()

	c.connected = false
	if c.conn != nil {
		// Bound the close-handshake write: WriteMessage has no deadline by
		// default, so on a half-dead connection (e.g. after macOS sleep or a
		// network change) it can block indefinitely. That stalls shutdown via
		// chat.Client.Close() (issue #312). A short deadline makes the write
		// fail fast; we then close the underlying TCP conn regardless.
		//
		// SetWriteDeadline is itself one of gorilla's write methods (it is a
		// bare field assignment on the Conn), so it and the close frame both
		// have to run under writeMu, otherwise shutdown races an in-flight
		// sendMessage. The acquire is bounded so a wedged writer cannot
		// reintroduce the #312 stall; if we cannot get the lock we skip the
		// courtesy close frame entirely. Closing the connection is documented
		// safe alongside any other method, and it unblocks the stuck writer.
		if c.tryLockWrite(closeWriteWait) {
			_ = c.conn.SetWriteDeadline(time.Now().Add(closeWriteWait))
			err := c.conn.WriteMessage(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			)
			c.writeMu.Unlock()
			if err != nil {
				c.debug("ws: error sending close message: %v", err)
			}
		} else {
			c.debug("ws: write in flight, closing without close handshake")
		}
		return c.conn.Close()
	}

	return nil
}
