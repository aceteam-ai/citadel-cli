// Package heartbeat provides periodic status reporting to the AceTeam control plane.
//
// This file implements Redis-based status publishing for real-time UI updates
// and reliable status processing via Redis Streams.
//
// Architecture:
//
//	Citadel Node                                Redis
//	┌─────────────┐    PUBLISH node:status:X   ┌─────────────┐
//	│   Redis     │ ────────────────────────▶  │  Pub/Sub    │ → Real-time UI
//	│  Publisher  │                            └─────────────┘
//	│   (30s)     │    XADD node:status:stream ┌─────────────┐
//	│             │ ────────────────────────▶  │  Streams    │ → Python Worker
//	└─────────────┘                            └─────────────┘
package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/redis/go-redis/v9"
)

// nodeIDPattern validates node IDs to prevent injection attacks.
// Only allows alphanumeric characters, hyphens, underscores, and dots.
var nodeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// StatusMessage is the payload published to Redis for status updates.
type StatusMessage struct {
	Version    string             `json:"version"`
	Timestamp  string             `json:"timestamp"`
	NodeID     string             `json:"nodeId"`
	DeviceCode string             `json:"deviceCode,omitempty"`
	Status     *status.NodeStatus `json:"status"`
}

// RedisPublisher publishes node status to Redis for real-time updates and reliable processing.
type RedisPublisher struct {
	client    *redis.Client
	redisURL  string // For debug logging
	nodeID    string
	interval  time.Duration
	collector *status.Collector

	// deviceCode is protected by mu since it can be updated after auth
	mu         sync.RWMutex
	deviceCode string

	// Redis key names
	pubSubChannel string // For real-time UI updates
	streamName    string // For reliable processing

	// Debug callback (optional)
	debugFunc func(format string, args ...any)

	// heartbeatCount tracks heartbeats to trigger keep-alive every 60s
	heartbeatCount int
}

// RedisPublisherConfig holds configuration for the Redis status publisher.
type RedisPublisherConfig struct {
	// RedisURL is the Redis connection URL
	RedisURL string

	// RedisPassword is the Redis password (optional)
	RedisPassword string

	// NodeID is the node identifier (typically hostname or Headscale node name)
	NodeID string

	// DeviceCode is the device authorization code for config lookup (optional)
	DeviceCode string

	// Interval is the time between status publishes (default: 30s)
	Interval time.Duration

	// ChannelOverride overrides the default pub/sub channel name (for debugging)
	// If empty, uses "node:status:{NodeID}"
	ChannelOverride string

	// DebugFunc is an optional callback for debug logging
	DebugFunc func(format string, args ...any)
}

// NewRedisPublisher creates a new Redis status publisher.
func NewRedisPublisher(cfg RedisPublisherConfig, collector *status.Collector) (*RedisPublisher, error) {
	if cfg.Interval == 0 {
		cfg.Interval = 30 * time.Second
	}

	// Validate NodeID to prevent injection attacks
	if cfg.NodeID != "" && !nodeIDPattern.MatchString(cfg.NodeID) {
		return nil, fmt.Errorf("invalid node ID: must be 1-64 alphanumeric characters, hyphens, underscores, or dots")
	}

	// Parse Redis URL
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Redis URL: %w", err)
	}

	if cfg.RedisPassword != "" {
		opts.Password = cfg.RedisPassword
	}

	client := redis.NewClient(opts)

	// Determine pub/sub channel name
	pubSubChannel := cfg.ChannelOverride
	if pubSubChannel == "" {
		pubSubChannel = fmt.Sprintf("node:status:%s", cfg.NodeID)
	}

	return &RedisPublisher{
		client:        client,
		redisURL:      cfg.RedisURL,
		nodeID:        cfg.NodeID,
		deviceCode:    cfg.DeviceCode,
		interval:      cfg.Interval,
		collector:     collector,
		pubSubChannel: pubSubChannel,
		streamName:    "node:status:stream",
		debugFunc:     cfg.DebugFunc,
	}, nil
}

// debug logs a message if debug function is configured
func (p *RedisPublisher) debug(format string, args ...any) {
	if p.debugFunc != nil {
		p.debugFunc(format, args...)
	}
}

// Start begins publishing status periodically to Redis.
// This method blocks until the context is cancelled.
func (p *RedisPublisher) Start(ctx context.Context) error {
	p.debug("starting Redis publisher")
	p.debug("redis: %s", p.redisURL)
	p.debug("nodeId: %s", p.nodeID)
	p.debug("pub/sub channel: %s", p.pubSubChannel)
	p.debug("stream: %s", p.streamName)
	p.debug("interval: %s", p.interval)

	// Verify connection
	p.debug("pinging Redis...")
	if err := p.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}
	p.debug("Redis ping successful")

	// Send initial status immediately
	p.debug("sending initial heartbeat...")
	if err := p.publishStatus(ctx); err != nil {
		fmt.Printf("   - Warning: Initial Redis status publish failed: %v\n", err)
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.debug("context cancelled, stopping publisher")
			return ctx.Err()
		case <-ticker.C:
			if err := p.publishStatus(ctx); err != nil {
				fmt.Printf("   - Warning: Redis status publish failed: %v\n", err)
			}
			// Trigger network keep-alive every 60s (every 2nd heartbeat at 30s interval)
			p.heartbeatCount++
			if p.heartbeatCount%2 == 0 {
				if err := network.KeepAlive(ctx); err != nil {
					p.debug("network keep-alive failed: %v", err)
				}
			}
		}
	}
}

// publishStatus collects status and publishes to both Pub/Sub and Streams.
func (p *RedisPublisher) publishStatus(ctx context.Context) error {
	// Collect current status
	nodeStatus, err := p.collector.CollectCompact()
	if err != nil {
		return fmt.Errorf("failed to collect status: %w", err)
	}

	// Get device code (thread-safe)
	deviceCode := p.getDeviceCode()

	// Build status message
	msg := StatusMessage{
		Version:    "1.0",
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		NodeID:     p.nodeID,
		DeviceCode: deviceCode,
		Status:     nodeStatus,
	}

	// Marshal to JSON
	jsonData, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal status: %w", err)
	}

	p.debug("heartbeat: publishing to channel %s", p.pubSubChannel)
	p.debug("heartbeat: nodeId=%s, deviceCode=%s, timestamp=%s", msg.NodeID, msg.DeviceCode, msg.Timestamp)
	p.debug("heartbeat: payload (%d bytes): %s", len(jsonData), string(jsonData))

	// Publish to Pub/Sub for real-time UI updates
	if err := p.client.Publish(ctx, p.pubSubChannel, jsonData).Err(); err != nil {
		return fmt.Errorf("failed to publish to Pub/Sub: %w", err)
	}
	p.debug("heartbeat: pub/sub publish successful")

	// Add to Stream for reliable processing
	streamFields := map[string]any{
		"nodeId":    p.nodeID,
		"timestamp": msg.Timestamp,
		"payload":   string(jsonData),
	}
	if deviceCode != "" {
		streamFields["deviceCode"] = deviceCode
	}

	if err := p.client.XAdd(ctx, &redis.XAddArgs{
		Stream: p.streamName,
		Values: streamFields,
		MaxLen: 10000, // Keep last 10k messages to prevent unbounded growth
		Approx: true,  // Approximate trimming for performance
	}).Err(); err != nil {
		return fmt.Errorf("failed to add to stream: %w", err)
	}
	p.debug("heartbeat: stream XADD successful")

	return nil
}

// SetDeviceCode updates the device code (used after device auth completes).
// This method is thread-safe.
func (p *RedisPublisher) SetDeviceCode(code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deviceCode = code
}

// getDeviceCode returns the device code in a thread-safe manner.
func (p *RedisPublisher) getDeviceCode() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.deviceCode
}

// PublishOnce sends a single status update and returns.
// Useful for testing or one-time status updates.
func (p *RedisPublisher) PublishOnce(ctx context.Context) error {
	return p.publishStatus(ctx)
}

// Close closes the Redis connection.
func (p *RedisPublisher) Close() error {
	if p.client != nil {
		return p.client.Close()
	}
	return nil
}

// NodeID returns the configured node ID.
func (p *RedisPublisher) NodeID() string {
	return p.nodeID
}

// Interval returns the configured publish interval.
func (p *RedisPublisher) Interval() time.Duration {
	return p.interval
}

// PubSubChannel returns the Pub/Sub channel name.
func (p *RedisPublisher) PubSubChannel() string {
	return p.pubSubChannel
}

// StreamName returns the Stream name.
func (p *RedisPublisher) StreamName() string {
	return p.streamName
}
