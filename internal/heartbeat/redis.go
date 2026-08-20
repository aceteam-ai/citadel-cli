// Package heartbeat provides periodic status reporting to the AceTeam control plane.
//
// This file implements Redis-based status publishing for real-time UI updates
// and reliable status processing via Redis Streams.
//
// Architecture (durable write first; see APIPublisher.publishMessage for why):
//
//	Citadel Node                                Redis
//	┌─────────────┐    XADD node:status:stream ┌─────────────┐
//	│   Redis     │ ────────────────────────▶  │  Streams    │ → Python Worker (durable)
//	│  Publisher  │                            └─────────────┘
//	│   (30s)     │    PUBLISH node:status:X   ┌─────────────┐
//	│             │ ────────────────────────▶  │  Pub/Sub    │ → Real-time UI (best effort)
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
	"github.com/aceteam-ai/citadel-cli/internal/pulse"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/redis/go-redis/v9"
)

// nodeIDPattern validates node IDs to prevent injection attacks.
// Only allows alphanumeric characters, hyphens, underscores, and dots.
var nodeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// StatusMessage is the payload published to Redis for status updates.
type StatusMessage struct {
	Version         string             `json:"version"`
	Timestamp       string             `json:"timestamp"`
	NodeID          string             `json:"nodeId"`
	HeadscaleNodeID string             `json:"headscaleNodeId,omitempty"`
	DeviceCode      string             `json:"deviceCode,omitempty"`
	Status          *status.NodeStatus `json:"status"`
	Permissions     *PermissionState   `json:"permissions,omitempty"`
	// Stats is the compact Fabric Pulse block (GPU + inference internals,
	// citadel-cli#587). Optional and versioned: legacy backends ignore it,
	// legacy nodes omit it. Read from the pulse collector's cache via
	// SetStatsProvider — never collected inline on the heartbeat path.
	Stats *pulse.StatsBlock `json:"stats,omitempty"`
}

// PermissionState mirrors config.Permissions for the heartbeat payload.
// Kept separate to avoid coupling the heartbeat wire format to the config package.
type PermissionState struct {
	Console  bool `json:"console"`
	Desktop  bool `json:"desktop"`
	Files    bool `json:"files"`
	Services bool `json:"services"`
	SSH      bool `json:"ssh"`
	Shell    bool `json:"shell"`

	// HasPasscode reports whether the node currently has a passcode set
	// (config.Permissions.HasPasscode), NOT the hash or plaintext itself
	// (citadel #758). Without this, a remote controller (dashboard/MCP) can
	// only infer "is a passcode set" from its own dispatch history, which
	// goes stale the moment a passcode is set/cleared directly on the node
	// (`citadel passcode set`, #755) or out of band. No omitempty: false is
	// the meaningful "not set" state, not an absent field — same as the
	// other capability bools in this struct.
	HasPasscode bool `json:"has_passcode"`
}

// RedisPublisher publishes node status to Redis for real-time updates and reliable processing.
type RedisPublisher struct {
	client          *redis.Client
	redisURL        string // For debug logging
	nodeID          string
	headscaleNodeID string // Headscale numeric node ID (e.g., "758")
	interval        time.Duration
	collector       *status.Collector

	// deviceCode is protected by mu since it can be updated after auth
	mu         sync.RWMutex
	deviceCode string

	// Redis key names
	pubSubChannel string // For real-time UI updates
	streamName    string // For reliable processing

	// Debug callback (optional)
	debugFunc func(format string, args ...any)

	// Log callback (optional, for TUI mode)
	logFn func(level, msg string)

	// permissions is included in heartbeats so the web UI knows which capabilities
	// the operator has enabled. Set via SetPermissions (a one-time snapshot) or,
	// preferably, SetPermissionsProvider (re-read every publish). See
	// SetPermissionsProvider for why the snapshot form goes stale.
	permissions *PermissionState

	// permissionsFn, when set, is called fresh on every publish instead of
	// reading the static permissions snapshot above. See SetPermissionsProvider.
	permissionsFn func() *PermissionState

	// heartbeatCount tracks heartbeats to trigger keep-alive every 60s
	heartbeatCount int

	// pubSubHealth rate-limits the log noise from a failing best-effort
	// pub/sub publish while keeping a sustained outage visible (#722).
	pubSubHealth pubSubHealth

	// onStatus, when set, is invoked with each freshly collected status after a
	// successful publish, driving the config-gated auto-stop reconciler off the
	// heartbeat's existing collection (citadel #416). Optional; nil by default.
	onStatus func(*status.NodeStatus)

	// statsFn, when set, returns the latest cached Fabric Pulse stats block
	// (citadel-cli#587). It is a cache read — it must never block or error —
	// and may return nil (no stats field on that heartbeat). Optional.
	statsFn func() *pulse.StatsBlock

	// markerDir, when non-empty, is where the cross-process heartbeat
	// freshness marker is written after every publish attempt (#726; see
	// marker.go). Empty (the default, including in every test in this
	// package) means "do not write a marker" -- this package does not import
	// internal/platform and resolve a default itself (same standalone
	// convention as internal/mesh and internal/config's LoadEnergy/SaveEnergy,
	// which take a configDir parameter rather than resolving one), so a
	// caller that wants the marker written must set RedisPublisherConfig.MarkerDir
	// explicitly (cmd/work.go passes network.GetNodeConfigDir()).
	markerDir string
}

// SetStatsProvider registers the Fabric Pulse cached-stats reader
// (pulse.Collector.Latest). The heartbeat attaches whatever the cache holds;
// it never triggers collection itself, so a wedged collector degrades to a
// heartbeat without stats, not a late heartbeat.
func (p *RedisPublisher) SetStatsProvider(fn func() *pulse.StatsBlock) {
	p.statsFn = fn
}

// SetOnStatus registers a callback invoked with each collected status. Used to
// drive the config-gated auto-stop-when-idle reconciler (citadel #416).
func (p *RedisPublisher) SetOnStatus(fn func(*status.NodeStatus)) {
	p.onStatus = fn
}

// RedisPublisherConfig holds configuration for the Redis status publisher.
type RedisPublisherConfig struct {
	// RedisURL is the Redis connection URL
	RedisURL string

	// RedisPassword is the Redis password (optional)
	RedisPassword string

	// NodeID is the node identifier (typically hostname or Headscale node name)
	NodeID string

	// HeadscaleNodeID is the Headscale numeric node ID (e.g., "758").
	// When set, included in heartbeat messages so the Python worker can skip
	// the Headscale hostname-to-ID lookup.
	HeadscaleNodeID string

	// DeviceCode is the device authorization code for config lookup (optional)
	DeviceCode string

	// Interval is the time between status publishes (default: 30s)
	Interval time.Duration

	// ChannelOverride overrides the default pub/sub channel name (for debugging)
	// If empty, uses "node:status:{NodeID}"
	ChannelOverride string

	// DebugFunc is an optional callback for debug logging
	DebugFunc func(format string, args ...any)

	// LogFn is an optional callback for logging (if nil, prints to stdout)
	LogFn func(level, msg string)

	// MarkerDir, when set, is where the cross-process heartbeat freshness
	// marker is written after every publish attempt so `citadel status` can
	// read it (citadel-cli#726). Callers pass network.GetNodeConfigDir(); left
	// empty (as every test in this package does), no marker is written.
	MarkerDir string
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
		client:          client,
		redisURL:        cfg.RedisURL,
		nodeID:          cfg.NodeID,
		headscaleNodeID: cfg.HeadscaleNodeID,
		deviceCode:      cfg.DeviceCode,
		interval:        cfg.Interval,
		collector:       collector,
		pubSubChannel:   pubSubChannel,
		streamName:      "node:status:stream",
		debugFunc:       cfg.DebugFunc,
		logFn:           cfg.LogFn,
		markerDir:       cfg.MarkerDir,
	}, nil
}

// debug logs a message if debug function is configured
func (p *RedisPublisher) debug(format string, args ...any) {
	if p.debugFunc != nil {
		p.debugFunc(format, args...)
	}
}

// log outputs a message - uses logFn callback if set, otherwise prints to stdout.
func (p *RedisPublisher) log(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if p.logFn != nil {
		p.logFn(level, msg)
	} else {
		fmt.Printf("%s\n", msg)
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
		p.log("warning", "   - Warning: Initial Redis status publish failed: %v", err)
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
				p.log("warning", "   - Warning: Redis status publish failed: %v", err)
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

	// Feed the collected status to any registered observer (the auto-stop
	// reconciler) before publishing, reusing this collection rather than
	// triggering a second stats/nvidia-smi sweep.
	if p.onStatus != nil {
		p.onStatus(nodeStatus)
	}

	// Get device code (thread-safe)
	deviceCode := p.getDeviceCode()

	// Build status message
	msg := StatusMessage{
		Version:         "1.0",
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		NodeID:          p.nodeID,
		HeadscaleNodeID: p.headscaleNodeID,
		DeviceCode:      deviceCode,
		Status:          nodeStatus,
		Permissions:     p.currentPermissions(),
	}
	if p.statsFn != nil {
		msg.Stats = p.statsFn()
	}

	return p.publishMessage(ctx, msg, deviceCode)
}

// publishMessage writes one heartbeat to both destinations. Split out of
// publishStatus so the write half is testable without a live status collector.
func (p *RedisPublisher) publishMessage(ctx context.Context, msg StatusMessage, deviceCode string) error {
	// Marshal to JSON
	jsonData, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal status: %w", err)
	}

	p.debug("heartbeat: nodeId=%s, headscaleNodeId=%s, deviceCode=%s, timestamp=%s", msg.NodeID, msg.HeadscaleNodeID, msg.DeviceCode, msg.Timestamp)
	p.debug("heartbeat: payload (%d bytes): %s", len(jsonData), string(jsonData))

	// Durable stream FIRST, and it owns the error. See the APIPublisher's
	// publishMessage for the full rationale (citadel-cli#722): the stream is
	// what the platform's NodeStatusWorker consumes to update the node's
	// last-reported timestamp, while the pub/sub publish only drives live UI
	// freshness. Making the best-effort write a gate on the reliable one is
	// what dropped a healthy node out of the fabric.
	//
	// Unlike the API publisher, both writes here share ONE Redis connection, so
	// their failures are strongly correlated and this ordering is a correctness
	// and consistency fix rather than a fix for an observed failure mode.
	streamFields := map[string]any{
		"nodeId":    p.nodeID,
		"timestamp": msg.Timestamp,
		"payload":   string(jsonData),
	}
	if deviceCode != "" {
		streamFields["deviceCode"] = deviceCode
	}

	streamErr := p.client.XAdd(ctx, &redis.XAddArgs{
		Stream: p.streamName,
		Values: streamFields,
		MaxLen: 10000, // Keep last 10k messages to prevent unbounded growth
		Approx: true,  // Approximate trimming for performance
	}).Err()
	if streamErr == nil {
		p.debug("heartbeat: stream XADD successful")
	}
	// Cross-process freshness marker for `citadel status` (#726): this
	// process (a long-running `citadel work`) is the only writer, and status
	// is a separate short-lived invocation with no other handle on it.
	// Best-effort -- never lets a marker-write failure turn a successful
	// heartbeat into a reported failure. Skipped entirely when markerDir is
	// unset (every test in this package; production sets it via
	// RedisPublisherConfig.MarkerDir).
	if p.markerDir != "" {
		now := time.Now()
		if streamErr == nil {
			_ = RecordSuccess(p.markerDir, now)
		} else {
			_ = RecordFailure(p.markerDir, now, streamErr)
		}
	}

	// Best-effort Pub/Sub for real-time UI updates. Never fatal.
	p.debug("heartbeat: publishing to channel %s", p.pubSubChannel)
	if pubErr := p.client.Publish(ctx, p.pubSubChannel, jsonData).Err(); pubErr != nil {
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

// SetPermissions updates the permission state included in heartbeats as a
// one-time snapshot. Prefer SetPermissionsProvider: a snapshot never reflects
// a permission (or the node passcode) changed after this call, e.g. by
// `citadel passcode set`/`clear` run in a separate process, or a Control
// Center edit, while this worker keeps running (citadel #758).
func (p *RedisPublisher) SetPermissions(perms *PermissionState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.permissions = perms
}

// SetPermissionsProvider registers a function re-read on every publish
// instead of a static snapshot, so heartbeats reflect the CURRENT permissions
// state (including HasPasscode) rather than whatever was set at worker
// startup. Mirrors SetStatsProvider's cache-read contract: fn must be cheap
// and non-blocking (config.LoadPermissions is a small YAML file read) and may
// return nil.
func (p *RedisPublisher) SetPermissionsProvider(fn func() *PermissionState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.permissionsFn = fn
}

// currentPermissions resolves the permissions to attach to this heartbeat:
// the live provider if one is registered, else the static snapshot.
func (p *RedisPublisher) currentPermissions() *PermissionState {
	p.mu.RLock()
	fn := p.permissionsFn
	static := p.permissions
	p.mu.RUnlock()
	if fn != nil {
		return fn()
	}
	return static
}
