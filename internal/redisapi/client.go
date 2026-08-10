package redisapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Client provides HTTP-based access to the AceTeam Redis API proxy.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
	workerID   string

	// wsClient is the WebSocket client for real-time pub/sub. It is built once
	// in NewClient and NEVER reassigned or nil'd afterwards.
	//
	// It used to be created lazily by EnableWebSocket and set back to nil when
	// the connect failed, which is what made a single failed connect permanent:
	// with no object left there was nothing to retry (issue #723). Owning it for
	// the Client's whole life means EnableWebSocket is a safe, idempotent
	// "connect if not connected" that a background retry loop can call, and the
	// field is immutable so readers (Publish) need no lock.
	//
	// Connected-ness, not existence, is the gate everywhere: a non-nil wsClient
	// that has never dialed reports IsConnected()==false and Publish falls back
	// to HTTP exactly as before.
	wsClient *WSClient

	// closed is set by Close so a retry loop that is still in flight cannot
	// resurrect the WebSocket after shutdown has begun.
	closed atomic.Bool

	// Debug callback (optional)
	debugFunc func(format string, args ...any)

	// Node identity metadata injected into stream events
	nodeMeta *NodeMeta

	// lastConsumeStatus is the HTTP status code of the most recent
	// jobs/consume request. Exposed via LastConsumeStatus() so the worker
	// introspection path can report it (issue #236). The pre-fix #3924 bug
	// surfaced here as repeated 400s with no visible error in the log file.
	lastConsumeStatus int32
}

// ClientConfig holds configuration for the API client.
type ClientConfig struct {
	// BaseURL is the AceTeam API base URL (e.g., "https://aceteam.ai")
	BaseURL string

	// Token is the device_api_token from device authentication
	Token string

	// Timeout is the HTTP request timeout (default: 30s)
	Timeout time.Duration

	// DebugFunc is an optional callback for debug logging
	DebugFunc func(format string, args ...any)
}

// NewClient creates a new Redis API client.
func NewClient(cfg ClientConfig) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}

	c := &Client{
		baseURL: cfg.BaseURL,
		token:   cfg.Token,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
		workerID:  fmt.Sprintf("citadel-%s", uuid.New().String()[:8]),
		debugFunc: cfg.DebugFunc,
	}

	// Build the WebSocket client up front (it does not dial until Connect).
	// See the wsClient field comment: owning it for the Client's lifetime is
	// what makes a failed connect retryable instead of terminal (issue #723).
	c.wsClient = NewWSClient(WSClientConfig{
		BaseURL:          cfg.BaseURL,
		Token:            cfg.Token,
		ReconnectEnabled: true,
		DebugFunc:        cfg.DebugFunc,
	})

	return c
}

// debug logs a message if debug function is configured
func (c *Client) debug(format string, args ...any) {
	if c.debugFunc != nil {
		c.debugFunc(format, args...)
	}
}

// BaseURL returns the API base URL.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// WorkerID returns the unique worker identifier.
func (c *Client) WorkerID() string {
	return c.workerID
}

// LastConsumeStatus returns the HTTP status code of the most recent
// jobs/consume request, or 0 if no consume request has been made yet.
// Used by the worker introspection path (issue #236) to report whether the
// consume loop is succeeding (200) or being rejected (e.g. 400/401/403).
func (c *Client) LastConsumeStatus() int {
	return int(atomic.LoadInt32(&c.lastConsumeStatus))
}

// Ping verifies the API connection by making a simple request.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/fabric/redis/ping", nil)
	if err != nil {
		return fmt.Errorf("failed to create ping request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ping failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		// A 429 carries a structured rate-limit hint (retry_after/reset_at).
		// Return it as a typed error so the connect-retry loop can honor the
		// server's backoff instead of hammering into the daily quota (#443).
		if resp.StatusCode == http.StatusTooManyRequests {
			return parseRateLimitError(resp.StatusCode, string(body))
		}
		return fmt.Errorf("ping failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// Close closes the HTTP client and WebSocket if connected.
func (c *Client) Close() error {
	c.closed.Store(true)
	if c.wsClient != nil {
		return c.wsClient.Close()
	}
	return nil
}

// ErrClientClosed is returned by EnableWebSocket once Close has run, so a
// background retry loop that outlives shutdown cannot re-dial a closed client.
var ErrClientClosed = errors.New("redis API client is closed")

// EnableWebSocket connects the WebSocket client for real-time pub/sub.
// This allows Publish calls to use WebSocket instead of HTTP when connected.
//
// It is idempotent and safe to call repeatedly: when already connected it is a
// no-op, and when not connected it makes exactly one dial attempt and reports
// the outcome. That is deliberate -- the caller owns the retry policy (see
// cmd/work.go's enableWebSocketWithRetry, issue #723) so a failed connect backs
// off instead of hot-looping and never leaves the client permanently HTTP-only.
//
// On failure the WebSocket client is retained (not nil'd), so the very next
// call can succeed.
func (c *Client) EnableWebSocket(ctx context.Context) error {
	if c.closed.Load() {
		return ErrClientClosed
	}
	if c.wsClient == nil {
		return fmt.Errorf("WebSocket client not initialized")
	}
	if c.wsClient.IsConnected() {
		return nil
	}

	if err := c.wsClient.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect WebSocket: %w", err)
	}

	c.debug("WebSocket enabled for real-time pub/sub")
	return nil
}

// Pub/sub transport names reported by PubSubTransport.
const (
	PubSubTransportWebSocket = "websocket"
	PubSubTransportHTTP      = "http"
)

// PubSubTransport reports which transport Publish will currently use:
// "websocket" when the real-time connection is up, "http" when it is not and
// publishes fall back to the REST route.
//
// This exists so the degraded state is visible without a debugger: a node whose
// startup WebSocket connect failed used to be indistinguishable from a healthy
// one at default log level (issue #723). Surfaced through the worker liveness
// payload and `citadel status`.
func (c *Client) PubSubTransport() string {
	if c.IsWebSocketConnected() {
		return PubSubTransportWebSocket
	}
	return PubSubTransportHTTP
}

// WebSocket returns the WebSocket client if enabled, nil otherwise.
func (c *Client) WebSocket() *WSClient {
	return c.wsClient
}

// IsWebSocketConnected returns whether the WebSocket is currently connected.
func (c *Client) IsWebSocketConnected() bool {
	return c.wsClient != nil && c.wsClient.IsConnected()
}

// SetNodeMeta sets the node identity metadata that will be included in all stream events.
func (c *Client) SetNodeMeta(nodeID, nodeName string) {
	c.nodeMeta = &NodeMeta{NodeID: nodeID, NodeName: nodeName}
}

// FetchWorkerConfig retrieves worker configuration from the AceTeam API.
// Returns queue name, consumer group, and org ID for the authenticated device.
// If the endpoint returns 404 (not yet deployed), returns nil without error
// so callers can fall back to defaults.
func (c *Client) FetchWorkerConfig(ctx context.Context) (*WorkerConfigResponse, error) {
	var resp WorkerConfigResponse
	err := c.doRequest(ctx, "GET", "/api/fabric/worker-config", nil, &resp)
	if err != nil {
		// Treat 404 as "endpoint not deployed yet" — return nil so the caller
		// falls back to hardcoded defaults rather than failing.
		if errStr := err.Error(); len(errStr) > 0 {
			// Check for 404 in the error message (API error format)
			if contains404(errStr) {
				c.debug("worker-config endpoint not available (404), using defaults")
				return nil, nil
			}
		}
		return nil, fmt.Errorf("failed to fetch worker config: %w", err)
	}

	return &resp, nil
}

// contains404 checks if an error string indicates an HTTP 404 response.
func contains404(s string) bool {
	return len(s) > 0 && (strings.Contains(s, "status 404") || strings.Contains(s, "\"404\""))
}

// doRequest performs an HTTP request with authentication and JSON handling.
func (c *Client) doRequest(ctx context.Context, method, path string, body any, result any) error {
	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(jsonData)
		c.debug("request: %s %s - body: %s", method, path, string(jsonData))
	} else {
		c.debug("request: %s %s", method, path)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	c.debug("response: %d - %s", resp.StatusCode, string(respBody))

	// Record the consume HTTP status for worker introspection (issue #236).
	// This is the single signal that would have surfaced the pre-fix #3924
	// 400s, which otherwise only appeared as a generic "consume failed" error.
	if strings.Contains(path, "/jobs/consume") {
		atomic.StoreInt32(&c.lastConsumeStatus, int32(resp.StatusCode))
	}

	// Handle error responses.
	//
	// Every error here carries method, path, HTTP status and the (truncated)
	// response body, because the callers of this package deliberately no longer
	// second-guess a 2xx (see pubsub.go): this error string is the ONLY record of
	// why a call failed. Issue #721 was slow to diagnose precisely because the
	// old strings dropped the status and the path.
	//
	// The literal "status %d" substring is load-bearing: contains404 greps for it
	// so FetchWorkerConfig can fall back to defaults on a 404. Keep it verbatim.
	if resp.StatusCode >= 400 {
		var apiErr APIError
		if err := json.Unmarshal(respBody, &apiErr); err == nil && apiErr.Error != "" {
			apiErr.StatusCode = resp.StatusCode
			return fmt.Errorf("%s %s failed with status %d: API error: %s", method, path, resp.StatusCode, apiErr.Err())
		}
		return fmt.Errorf("%s %s failed with status %d: %s", method, path, resp.StatusCode, truncateBody(respBody))
	}

	// Parse success response
	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("%s %s returned status %d with an unparseable body (%s): %w",
				method, path, resp.StatusCode, truncateBody(respBody), err)
		}
	}

	return nil
}

// maxErrorBodyBytes bounds how much of a response body is quoted into an error
// string. Enough to carry a JSON error payload, small enough that a stray HTML
// error page does not flood the log.
const maxErrorBodyBytes = 512

// truncateBody renders a response body for inclusion in an error message.
func truncateBody(body []byte) string {
	if len(body) == 0 {
		return "<empty body>"
	}
	if len(body) > maxErrorBodyBytes {
		return string(body[:maxErrorBodyBytes]) + fmt.Sprintf("... (%d bytes total)", len(body))
	}
	return string(body)
}
