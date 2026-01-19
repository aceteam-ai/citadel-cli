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
	httpClient *http.Client
}

// DeregisterRequest represents the request body for the /deregister endpoint
type DeregisterRequest struct {
	OrgID    string `json:"org_id,omitempty"`
	NodeName string `json:"node_name,omitempty"`
}

// DeregisterResponse represents a successful deregister response
type DeregisterResponse struct {
	Status  string `json:"status"`
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

// NewDeregisterClient creates a new deregister client
func NewDeregisterClient(baseURL string) *DeregisterClient {
	return &DeregisterClient{
		baseURL: baseURL,
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
