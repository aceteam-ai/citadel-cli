// Package heartbeat provides periodic status reporting to the AceTeam control plane.
//
// This file implements API-based status publishing for real-time UI updates
// when using the secure Redis API proxy instead of direct Redis connections.
//
// Architecture:
//
//	Citadel Node                                  AceTeam API
//	┌─────────────┐    POST /redis/pubsub/publish ┌─────────────┐
//	│    API      │ ─────────────────────────────▶│  Redis API  │ → Redis Pub/Sub
//	│  Publisher  │                               │   Proxy     │
//	│   (30s)     │    POST /redis/streams/add    └─────────────┘
//	│             │ ─────────────────────────────▶│  Redis API  │ → Redis Streams
//	└─────────────┘                               └─────────────┘
package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// APIPublisher publishes node status via the Redis API proxy.
// This is the secure alternative to direct Redis connections.
type APIPublisher struct {
	client    *redisapi.Client
	nodeID    string
	orgID     string
	interval  time.Duration
	collector *status.Collector

	// Redis key names
	pubSubChannel string // format: node:status:org:{orgId}:{hostname}
	streamName    string // format: node:status:stream

	// Debug callback (optional)
	debugFunc func(format string, args ...any)
}

// APIPublisherConfig holds configuration for the API status publisher.
type APIPublisherConfig struct {
	// Client is the Redis API client (required)
	Client *redisapi.Client

	// NodeID is the node identifier (typically hostname or network node name)
	NodeID string

	// OrgID is the organization ID for channel scoping (required for API mode)
	OrgID string

	// Interval is the time between status publishes (default: 30s)
	Interval time.Duration

	// DebugFunc is an optional callback for debug logging
	DebugFunc func(format string, args ...any)
}

// NewAPIPublisher creates a new API-based status publisher.
func NewAPIPublisher(cfg APIPublisherConfig, collector *status.Collector) (*APIPublisher, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("Client is required")
	}
	if cfg.OrgID == "" {
		return nil, fmt.Errorf("OrgID is required for API mode")
	}

	if cfg.Interval == 0 {
		cfg.Interval = 30 * time.Second
	}

	// Validate NodeID to prevent injection attacks
	if cfg.NodeID != "" && !nodeIDPattern.MatchString(cfg.NodeID) {
		return nil, fmt.Errorf("invalid node ID: must be 1-64 alphanumeric characters, hyphens, underscores, or dots")
	}

	// Channel format for API: node:status:org:{orgId}:{hostname}
	pubSubChannel := fmt.Sprintf("node:status:org:%s:%s", cfg.OrgID, cfg.NodeID)

	return &APIPublisher{
		client:        cfg.Client,
		nodeID:        cfg.NodeID,
		orgID:         cfg.OrgID,
		interval:      cfg.Interval,
		collector:     collector,
		pubSubChannel: pubSubChannel,
		streamName:    "node:status:stream",
		debugFunc:     cfg.DebugFunc,
	}, nil
}

// debug logs a message if debug function is configured
func (p *APIPublisher) debug(format string, args ...any) {
	if p.debugFunc != nil {
		p.debugFunc(format, args...)
	}
}

// Start begins publishing status periodically via the API.
// This method blocks until the context is cancelled.
func (p *APIPublisher) Start(ctx context.Context) error {
	p.debug("starting API publisher")
	p.debug("nodeId: %s", p.nodeID)
	p.debug("orgId: %s", p.orgID)
	p.debug("pub/sub channel: %s", p.pubSubChannel)
	p.debug("interval: %s", p.interval)

	// Send initial status immediately
	p.debug("sending initial heartbeat...")
	if err := p.publishStatus(ctx); err != nil {
		fmt.Printf("   - Warning: Initial API status publish failed: %v\n", err)
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
				fmt.Printf("   - Warning: API status publish failed: %v\n", err)
			}
		}
	}
}

// publishStatus collects status and publishes to both Pub/Sub and Streams via API.
func (p *APIPublisher) publishStatus(ctx context.Context) error {
	// Collect current status
	nodeStatus, err := p.collector.CollectCompact()
	if err != nil {
		return fmt.Errorf("failed to collect status: %w", err)
	}

	timestamp := time.Now().UTC().Format(time.RFC3339)

	// Build status message
	msg := StatusMessage{
		Version:   "1.0",
		Timestamp: timestamp,
		NodeID:    p.nodeID,
		Status:    nodeStatus,
	}

	p.debug("heartbeat: publishing to channel %s", p.pubSubChannel)
	p.debug("heartbeat: nodeId=%s, timestamp=%s", msg.NodeID, msg.Timestamp)

	// 1. Publish to Pub/Sub for real-time UI updates
	if err := p.client.Publish(ctx, p.pubSubChannel, msg); err != nil {
		return fmt.Errorf("failed to publish to pub/sub: %w", err)
	}
	p.debug("heartbeat: pub/sub publish successful")

	// 2. Add to Stream for reliable processing by Python worker
	// Marshal the full message as payload
	payloadJSON, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	streamFields := map[string]string{
		"nodeId":    p.nodeID,
		"timestamp": timestamp,
		"payload":   string(payloadJSON),
	}

	if err := p.client.StreamAdd(ctx, p.streamName, streamFields, 10000); err != nil {
		// Log but don't fail - pub/sub already succeeded
		p.debug("heartbeat: stream add failed: %v", err)
		fmt.Printf("   - Warning: Stream add failed: %v\n", err)
	} else {
		p.debug("heartbeat: stream add successful")
	}

	return nil
}

// PublishOnce sends a single status update and returns.
// Useful for testing or one-time status updates.
func (p *APIPublisher) PublishOnce(ctx context.Context) error {
	return p.publishStatus(ctx)
}

// NodeID returns the configured node ID.
func (p *APIPublisher) NodeID() string {
	return p.nodeID
}

// OrgID returns the configured org ID.
func (p *APIPublisher) OrgID() string {
	return p.orgID
}

// Interval returns the configured publish interval.
func (p *APIPublisher) Interval() time.Duration {
	return p.interval
}

// PubSubChannel returns the Pub/Sub channel name.
func (p *APIPublisher) PubSubChannel() string {
	return p.pubSubChannel
}

// StreamName returns the Stream name.
func (p *APIPublisher) StreamName() string {
	return p.streamName
}
