// Package heartbeat provides periodic status reporting to the AceTeam control plane.
//
// The heartbeat client runs as a background goroutine and periodically sends
// node status to the AceTeam API. This enables the Fabric page to show
// near-real-time status without polling each node individually.
//
// Architecture:
//
//	Citadel Node                          AceTeam API
//	┌─────────────┐    POST /heartbeat    ┌─────────────┐
//	│  Heartbeat  │ ───────────────────▶  │  /api/fabric│
//	│  Client     │                       │  /nodes/:id │
//	│  (30s)      │  ◀─────────────────── │  /heartbeat │
//	└─────────────┘         200 OK        └─────────────┘
package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// Client sends periodic heartbeats to the AceTeam API.
type Client struct {
	endpoint   string        // Full URL including node ID
	interval   time.Duration // Time between heartbeats
	apiKey     string        // API key for authentication
	collector  *status.Collector
	httpClient *http.Client
	nodeID     string
	logFn      func(level, msg string)
}

// ClientConfig holds configuration for the heartbeat client.
type ClientConfig struct {
	// BaseURL is the AceTeam API base URL (e.g., "https://aceteam.ai")
	BaseURL string

	// NodeID is the Headscale node identifier
	NodeID string

	// Interval is the time between heartbeats (default: 30s)
	Interval time.Duration

	// APIKey is the authentication token for the heartbeat endpoint
	APIKey string

	// Timeout is the HTTP request timeout (default: 10s)
	Timeout time.Duration

	// LogFn is an optional callback for logging (if nil, prints to stdout)
	LogFn func(level, msg string)
}

// NewClient creates a new heartbeat client.
func NewClient(cfg ClientConfig, collector *status.Collector) *Client {
	if cfg.Interval == 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}

	endpoint := fmt.Sprintf("%s/api/fabric/nodes/%s/heartbeat", cfg.BaseURL, cfg.NodeID)

	return &Client{
		endpoint:  endpoint,
		interval:  cfg.Interval,
		apiKey:    cfg.APIKey,
		collector: collector,
		nodeID:    cfg.NodeID,
		logFn:     cfg.LogFn,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

// log outputs a message - uses logFn callback if set, otherwise prints to stdout.
func (c *Client) log(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if c.logFn != nil {
		c.logFn(level, msg)
	} else {
		fmt.Printf("%s\n", msg)
	}
}

// Start begins sending periodic heartbeats.
// This method blocks until the context is cancelled.
func (c *Client) Start(ctx context.Context) error {
	// Send initial heartbeat immediately
	if err := c.sendHeartbeat(ctx); err != nil {
		// Log but don't fail on first heartbeat error
		c.log("warning", "   - ⚠️ Initial heartbeat failed: %v", err)
	}

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.sendHeartbeat(ctx); err != nil {
				// Log errors but continue running
				c.log("warning", "   - ⚠️ Heartbeat failed: %v", err)
			}
		}
	}
}

// sendHeartbeat collects status and sends it to the API.
func (c *Client) sendHeartbeat(ctx context.Context) error {
	// Collect current status
	nodeStatus, err := c.collector.CollectCompact()
	if err != nil {
		return fmt.Errorf("failed to collect status: %w", err)
	}

	// Marshal to JSON
	body, err := json.Marshal(nodeStatus)
	if err != nil {
		return fmt.Errorf("failed to marshal status: %w", err)
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("X-Citadel-Node-ID", c.nodeID)

	// Send request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send heartbeat: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("heartbeat returned status %d", resp.StatusCode)
	}

	return nil
}

// SendOnce sends a single heartbeat and returns.
// Useful for testing or one-time status updates.
func (c *Client) SendOnce(ctx context.Context) error {
	return c.sendHeartbeat(ctx)
}

// Endpoint returns the configured heartbeat endpoint URL.
func (c *Client) Endpoint() string {
	return c.endpoint
}

// Interval returns the configured heartbeat interval.
func (c *Client) Interval() time.Duration {
	return c.interval
}
