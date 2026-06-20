package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// Client provides chat messaging over the AceTeam Redis API proxy.
//
// Real-time delivery uses WebSocket Pub/Sub (a dedicated connection, separate
// from the HTTP path — satisfying the "never block the shared connection"
// constraint). Message persistence uses StreamAdd via HTTP.
//
// History (XRANGE) is not yet available in the Redis API proxy, so only
// messages received while the client is subscribed are displayed. A future
// backend endpoint will enable loading the last N messages on connect.
type Client struct {
	api      *redisapi.Client
	orgID    string
	nodeID   string
	nodeName string

	// Callbacks
	onMessage  func(Message)
	onPresence func(PresenceInfo)
	mu         sync.RWMutex

	// Presence tracking
	peers   map[string]PresenceInfo
	peersMu sync.RWMutex

	// Lifecycle
	cancel context.CancelFunc
	done   chan struct{}
}

// ClientConfig holds configuration for the chat client.
type ClientConfig struct {
	// APIBaseURL is the AceTeam API base URL (e.g., "https://aceteam.ai").
	APIBaseURL string
	// Token is the device_api_token from device authentication.
	Token string
	// OrgID is the organization ID for channel scoping.
	OrgID string
	// NodeID is the Headscale node ID or hostname.
	NodeID string
	// NodeName is the human-readable node name.
	NodeName string
}

// NewClient creates a new chat client. Call Connect to start receiving messages.
func NewClient(cfg ClientConfig) *Client {
	apiClient := redisapi.NewClient(redisapi.ClientConfig{
		BaseURL: cfg.APIBaseURL,
		Token:   cfg.Token,
		Timeout: 30 * time.Second,
	})

	return &Client{
		api:      apiClient,
		orgID:    cfg.OrgID,
		nodeID:   cfg.NodeID,
		nodeName: cfg.NodeName,
		peers:    make(map[string]PresenceInfo),
		done:     make(chan struct{}),
	}
}

// OnMessage sets the callback invoked when a chat message arrives.
func (c *Client) OnMessage(fn func(Message)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onMessage = fn
}

// OnPresence sets the callback invoked when a presence update arrives.
func (c *Client) OnPresence(fn func(PresenceInfo)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onPresence = fn
}

// Connect establishes the WebSocket subscription for real-time messages
// and starts the presence heartbeat. Blocks until ctx is cancelled or
// Close is called.
func (c *Client) Connect(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	// Connect WebSocket (dedicated connection for subscriptions)
	if err := c.api.EnableWebSocket(ctx); err != nil {
		cancel()
		return fmt.Errorf("websocket connect: %w", err)
	}

	ws := c.api.WebSocket()
	if ws == nil {
		cancel()
		return fmt.Errorf("websocket not available after enable")
	}

	// Register message handler for incoming pub/sub messages
	ws.OnMessage("message", func(msg redisapi.WSMessage) {
		c.handleWSMessage(msg)
	})

	// Subscribe to chat channel and presence channel
	chatCh := ChannelName(c.orgID, "general")
	presCh := PresenceChannel(c.orgID)

	if err := ws.Subscribe(ctx, chatCh, presCh); err != nil {
		cancel()
		return fmt.Errorf("subscribe: %w", err)
	}

	// Start presence heartbeat
	go c.presenceLoop(ctx)

	// Wait for cancellation
	<-ctx.Done()
	close(c.done)
	return ctx.Err()
}

// Send publishes a chat message to the general channel.
func (c *Client) Send(ctx context.Context, body string) error {
	msg := Message{
		FromNodeID:   c.nodeID,
		FromNodeName: c.nodeName,
		Channel:      "general",
		Body:         body,
		Timestamp:    time.Now().UTC(),
	}

	chatCh := ChannelName(c.orgID, "general")

	// Publish via Pub/Sub for real-time delivery
	if err := c.api.Publish(ctx, chatCh, msg); err != nil {
		return fmt.Errorf("publish message: %w", err)
	}

	// Persist to stream for history (best-effort; no read endpoint yet)
	msgJSON, err := json.Marshal(msg)
	if err == nil {
		streamKey := StreamName(c.orgID, "general")
		_ = c.api.StreamAdd(ctx, streamKey, map[string]string{
			"data": string(msgJSON),
		}, 100)
	}

	return nil
}

// Peers returns a snapshot of currently tracked peers.
func (c *Client) Peers() []PresenceInfo {
	c.peersMu.RLock()
	defer c.peersMu.RUnlock()

	peers := make([]PresenceInfo, 0, len(c.peers))
	for _, p := range c.peers {
		peers = append(peers, p)
	}
	return peers
}

// OnlinePeers returns peers seen within the given timeout.
func (c *Client) OnlinePeers(timeout time.Duration) []PresenceInfo {
	c.peersMu.RLock()
	defer c.peersMu.RUnlock()

	peers := make([]PresenceInfo, 0, len(c.peers))
	for _, p := range c.peers {
		if p.IsOnline(timeout) {
			peers = append(peers, p)
		}
	}
	return peers
}

// Close shuts down the chat client.
func (c *Client) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	if c.api != nil {
		return c.api.Close()
	}
	return nil
}

// handleWSMessage dispatches incoming WebSocket messages to the appropriate callback.
func (c *Client) handleWSMessage(msg redisapi.WSMessage) {
	if msg.Message == nil {
		return
	}

	channel := msg.Channel

	// Determine if this is a presence or chat message based on channel
	if channel == PresenceChannel(c.orgID) {
		p, err := UnmarshalPresenceFromMap(msg.Message)
		if err != nil {
			return
		}
		c.peersMu.Lock()
		c.peers[p.NodeID] = p
		c.peersMu.Unlock()

		c.mu.RLock()
		fn := c.onPresence
		c.mu.RUnlock()
		if fn != nil {
			fn(p)
		}
		return
	}

	// Chat message
	chatMsg, err := UnmarshalMessageFromMap(msg.Message)
	if err != nil {
		return
	}

	c.mu.RLock()
	fn := c.onMessage
	c.mu.RUnlock()
	if fn != nil {
		fn(chatMsg)
	}
}

// presenceLoop publishes this node's presence every 30 seconds.
func (c *Client) presenceLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Publish immediately on start
	c.publishPresence(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.publishPresence(ctx)
		}
	}
}

// publishPresence sends a presence heartbeat.
func (c *Client) publishPresence(ctx context.Context) {
	info := PresenceInfo{
		NodeID:   c.nodeID,
		NodeName: c.nodeName,
		LastSeen: time.Now().UTC(),
	}

	presCh := PresenceChannel(c.orgID)
	_ = c.api.Publish(ctx, presCh, info)
}
