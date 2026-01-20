package redisapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Client provides HTTP-based access to the AceTeam Redis API proxy.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
	workerID   string

	// Debug callback (optional)
	debugFunc func(format string, args ...any)
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

	return &Client{
		baseURL: cfg.BaseURL,
		token:   cfg.Token,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
		workerID:  fmt.Sprintf("citadel-%s", uuid.New().String()[:8]),
		debugFunc: cfg.DebugFunc,
	}
}

// debug logs a message if debug function is configured
func (c *Client) debug(format string, args ...any) {
	if c.debugFunc != nil {
		c.debugFunc(format, args...)
	}
}

// WorkerID returns the unique worker identifier.
func (c *Client) WorkerID() string {
	return c.workerID
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
		return fmt.Errorf("ping failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// Close closes the HTTP client (no-op for now, but provided for interface consistency).
func (c *Client) Close() error {
	return nil
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

	// Handle error responses
	if resp.StatusCode >= 400 {
		var apiErr APIError
		if err := json.Unmarshal(respBody, &apiErr); err == nil && apiErr.Error != "" {
			apiErr.StatusCode = resp.StatusCode
			return fmt.Errorf("API error: %s", apiErr.Err())
		}
		return fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse success response
	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("failed to parse response: %w", err)
		}
	}

	return nil
}
