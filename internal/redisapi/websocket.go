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

	// Message handlers
	handlers   map[string]func(WSMessage)
	handlersMu sync.RWMutex

	// Subscriptions to restore on reconnect
	subscriptions   map[string]bool
	subscriptionsMu sync.RWMutex

	// Control channels
	done     chan struct{}
	stopOnce sync.Once

	// Reconnection settings
	reconnectEnabled bool
	reconnectBackoff time.Duration
	maxBackoff       time.Duration

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
}

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
		reconnectBackoff: time.Second,
		maxBackoff:       time.Minute,
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
			return fmt.Errorf("WebSocket connection failed with status %d: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("WebSocket connection failed: %w", err)
	}

	c.conn = conn
	c.connected = true
	c.reconnectBackoff = time.Second // Reset backoff on successful connection

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
			c.handleDisconnect()
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

// handleDisconnect handles WebSocket disconnection
func (c *WSClient) handleDisconnect() {
	c.connMu.Lock()
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

		c.debug("ws: attempting reconnect in %v", c.reconnectBackoff)
		time.Sleep(c.reconnectBackoff)

		// Increase backoff for next attempt
		c.reconnectBackoff *= 2
		if c.reconnectBackoff > c.maxBackoff {
			c.reconnectBackoff = c.maxBackoff
		}

		c.connMu.Lock()
		err := c.connectLocked(context.Background())
		c.connMu.Unlock()

		if err != nil {
			c.debug("ws: reconnect failed: %v", err)
			continue
		}

		c.debug("ws: reconnected successfully")
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

	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	return nil
}

// OnMessage registers a handler for a specific message type.
// Use "*" to handle all message types.
func (c *WSClient) OnMessage(msgType string, handler func(WSMessage)) {
	c.handlersMu.Lock()
	c.handlers[msgType] = handler
	c.handlersMu.Unlock()
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
		err := c.conn.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		)
		if err != nil {
			c.debug("ws: error sending close message: %v", err)
		}
		return c.conn.Close()
	}

	return nil
}
