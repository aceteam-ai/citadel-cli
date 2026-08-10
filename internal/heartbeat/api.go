// Package heartbeat provides periodic status reporting to the AceTeam control plane.
//
// This file implements API-based status publishing for real-time UI updates
// when using the secure Redis API proxy instead of direct Redis connections.
//
// Architecture (durable write first; see publishMessage for why):
//
//	Citadel Node                                  AceTeam API
//	┌─────────────┐    POST /redis/streams/add    ┌─────────────┐
//	│    API      │ ─────────────────────────────▶│  Redis API  │ → Redis Streams (durable)
//	│  Publisher  │                               │   Proxy     │
//	│   (30s)     │    POST /redis/pubsub/publish └─────────────┘
//	│             │ ─────────────────────────────▶│  Redis API  │ → Redis Pub/Sub (best effort)
//	└─────────────┘                               └─────────────┘
package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/pulse"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// APIPublisher publishes node status via the Redis API proxy.
// This is the secure alternative to direct Redis connections.
type APIPublisher struct {
	client          *redisapi.Client
	nodeID          string
	headscaleNodeID string // Headscale numeric node ID (e.g., "758")
	orgID           string
	interval        time.Duration
	collector       *status.Collector

	// Redis key names
	pubSubChannel string // format: node:status:org:{orgId}:{hostname}
	streamName    string // format: node:status:stream

	// permissions is included in heartbeats so the web UI knows which capabilities
	// the operator has enabled. Set via SetPermissions.
	permissions *PermissionState

	// Debug callback (optional)
	debugFunc func(format string, args ...any)

	// Log callback (optional, for TUI mode)
	logFn func(level, msg string)

	// heartbeatCount tracks heartbeats to trigger keep-alive every 60s
	heartbeatCount int

	// pubSubHealth rate-limits the log noise from a failing best-effort
	// pub/sub publish while keeping a sustained outage visible (#722).
	pubSubHealth pubSubHealth

	// onStatus, when set, is invoked with each freshly collected status after a
	// successful publish. It lets an auto-stop reconciler act on the exact state
	// that was just published without triggering a second (expensive) collection
	// pass on an already-loaded node. Optional; nil by default. See citadel #416.
	onStatus func(*status.NodeStatus)

	// statsFn, when set, returns the latest cached Fabric Pulse stats block
	// (citadel-cli#587). It is a cache read — it must never block or error —
	// and may return nil (no stats field on that heartbeat). Optional.
	statsFn func() *pulse.StatsBlock
}

// SetStatsProvider registers the Fabric Pulse cached-stats reader
// (pulse.Collector.Latest). The heartbeat attaches whatever the cache holds;
// it never triggers collection itself, so a wedged collector degrades to a
// heartbeat without stats, not a late heartbeat.
func (p *APIPublisher) SetStatsProvider(fn func() *pulse.StatsBlock) {
	p.statsFn = fn
}

// SetOnStatus registers a callback invoked with each collected status. Used to
// drive the config-gated auto-stop-when-idle reconciler (citadel #416) off the
// heartbeat's existing collection, so enabling it adds no extra docker/nvidia-smi
// execs.
func (p *APIPublisher) SetOnStatus(fn func(*status.NodeStatus)) {
	p.onStatus = fn
}

// APIPublisherConfig holds configuration for the API status publisher.
type APIPublisherConfig struct {
	// Client is the Redis API client (required)
	Client *redisapi.Client

	// NodeID is the node identifier (typically hostname or network node name)
	NodeID string

	// HeadscaleNodeID is the Headscale numeric node ID (e.g., "758").
	// When set, included in heartbeat messages so the Python worker can skip
	// the Headscale hostname-to-ID lookup.
	HeadscaleNodeID string

	// OrgID is the organization ID for channel scoping (required for API mode)
	OrgID string

	// Interval is the time between status publishes (default: 30s)
	Interval time.Duration

	// DebugFunc is an optional callback for debug logging
	DebugFunc func(format string, args ...any)

	// LogFn is an optional callback for logging (if nil, prints to stdout)
	LogFn func(level, msg string)
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
		client:          cfg.Client,
		nodeID:          cfg.NodeID,
		headscaleNodeID: cfg.HeadscaleNodeID,
		orgID:           cfg.OrgID,
		interval:        cfg.Interval,
		collector:       collector,
		pubSubChannel:   pubSubChannel,
		streamName:      "node:status:stream",
		debugFunc:       cfg.DebugFunc,
		logFn:           cfg.LogFn,
	}, nil
}

// debug logs a message if debug function is configured
func (p *APIPublisher) debug(format string, args ...any) {
	if p.debugFunc != nil {
		p.debugFunc(format, args...)
	}
}

// log outputs a message - uses logFn callback if set, otherwise prints to stdout.
func (p *APIPublisher) log(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if p.logFn != nil {
		p.logFn(level, msg)
	} else {
		fmt.Printf("%s\n", msg)
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
		p.log("warning", "   - Warning: Initial API status publish failed: %v", err)
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
				p.log("warning", "   - Warning: API status publish failed: %v", err)
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

// publishStatus collects status and publishes to both Pub/Sub and Streams via API.
func (p *APIPublisher) publishStatus(ctx context.Context) error {
	// Collect current status
	nodeStatus, err := p.collector.CollectCompact()
	if err != nil {
		return fmt.Errorf("failed to collect status: %w", err)
	}

	// Feed the collected status to any registered observer (the auto-stop
	// reconciler) before publishing. Reusing this collection avoids a second
	// docker stats / nvidia-smi sweep on a contended node.
	if p.onStatus != nil {
		p.onStatus(nodeStatus)
	}

	timestamp := time.Now().UTC().Format(time.RFC3339)

	// Build status message
	msg := StatusMessage{
		Version:         "1.0",
		Timestamp:       timestamp,
		NodeID:          p.nodeID,
		HeadscaleNodeID: p.headscaleNodeID,
		Status:          nodeStatus,
		Permissions:     p.permissions,
	}
	if p.statsFn != nil {
		msg.Stats = p.statsFn()
	}

	return p.publishMessage(ctx, msg, timestamp)
}

// publishMessage writes one heartbeat to both destinations. It is split out of
// publishStatus so the write half is testable without a live status collector.
//
// The two writes are deliberately asymmetric in BOTH order and severity
// (citadel-cli#722):
//
//   - The durable stream (`node:status:stream`) goes FIRST and owns the error.
//     It is what the platform's NodeStatusWorker consumes to upsert
//     fabric_node_status, which is the node's last-reported timestamp, which
//     fail-closed per-node routing checks before dispatching to this node. If
//     it does not land, the node leaves the fabric. Writing it first also means
//     a kill between the two calls still leaves the durable record written.
//
//   - The pub/sub publish is best effort. It only feeds live dashboard
//     freshness (and even that redundantly: the platform worker republishes to
//     the same org-scoped channel after its DB upsert). A failure here is
//     reported, never fatal.
//
// The previous order made the best-effort write a hard gate on the reliable
// one: a pub/sub failure returned early, the XADD never ran, and a healthy node
// read offline for 12 hours.
func (p *APIPublisher) publishMessage(ctx context.Context, msg StatusMessage, timestamp string) error {
	payloadJSON, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	streamFields := map[string]string{
		"nodeId":    p.nodeID,
		"timestamp": timestamp,
		"payload":   string(payloadJSON),
	}

	// 1. Durable stream for reliable processing by the platform worker.
	// Attempted first; its failure is the only fatal one.
	streamErr := p.client.StreamAdd(ctx, p.streamName, streamFields, 10000)
	if streamErr == nil {
		p.debug("heartbeat: stream add successful")
	}

	// 2. Best-effort pub/sub for real-time UI updates. Attempted even when the
	// stream write failed, since the two paths fail independently in API mode.
	p.debug("heartbeat: publishing to channel %s", p.pubSubChannel)
	if pubErr := p.client.Publish(ctx, p.pubSubChannel, msg); pubErr != nil {
		p.debug("heartbeat: pub/sub publish failed (non-fatal): %v", pubErr)
		if report, escalate := p.pubSubHealth.recordFailure(time.Now()); escalate {
			p.log("warning", "   - Warning: heartbeat pub/sub publish to %s failing (%s): %v (live UI updates only; durable status stream unaffected)",
				p.pubSubChannel, report.describe(), pubErr)
		}
	} else {
		p.debug("heartbeat: pub/sub publish successful")
		if report, recovered := p.pubSubHealth.recordSuccess(time.Now()); recovered {
			p.log("info", "   - heartbeat pub/sub publish to %s recovered after %s",
				p.pubSubChannel, report.describe())
		}
	}

	if streamErr != nil {
		return fmt.Errorf("failed to add to durable stream %s: %w", p.streamName, streamErr)
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

// SetPermissions updates the permission state included in heartbeats.
func (p *APIPublisher) SetPermissions(perms *PermissionState) {
	p.permissions = perms
}
