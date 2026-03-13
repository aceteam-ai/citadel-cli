// internal/nexus/deregister.go
package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DeregisterClient handles node deregistration from Headscale
type DeregisterClient struct {
	baseURL    string
	apiToken   string
	httpClient *http.Client
}

// DeregisterRequest represents the request body for the /deregister endpoint
type DeregisterRequest struct {
	NodeName string `json:"node_name"`
}

// DeregisterResponse represents a successful deregister response
type DeregisterResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// DeregisterError represents an error response from the /deregister endpoint
type DeregisterError struct {
	ErrorCode   string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

func (e *DeregisterError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("%s: %s", e.ErrorCode, e.Description)
	}
	return e.ErrorCode
}

// NewDeregisterClient creates a new deregister client.
// apiToken is the Bearer token for authentication (e.g. CITADEL_API_KEY).
func NewDeregisterClient(baseURL, apiToken string) *DeregisterClient {
	return &DeregisterClient{
		baseURL:  baseURL,
		apiToken: apiToken,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Deregister removes the node from Headscale via the backend API.
// This is a best-effort operation - errors should be logged but not block logout.
func (c *DeregisterClient) Deregister(ctx context.Context, req DeregisterRequest) error {
	url := c.baseURL + "/api/fabric/device-auth/deregister"

	// Create request body
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiToken)
	}

	// Send request
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Handle response based on status code
	switch {
	case resp.StatusCode == http.StatusOK:
		// Success
		return nil

	case resp.StatusCode == http.StatusNotFound:
		// Node not found is also success (already deregistered)
		return nil

	case resp.StatusCode >= 400:
		// Try to parse error response
		var errResp DeregisterError
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil && errResp.ErrorCode != "" {
			return &errResp
		}
		return fmt.Errorf("server returned status %d", resp.StatusCode)

	default:
		return nil
	}
}
