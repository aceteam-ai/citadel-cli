package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AceTeamHarness provides test utilities for the AceTeam API
type AceTeamHarness struct {
	baseURL    string
	httpClient *http.Client
}

// NewAceTeamHarness creates a new AceTeam API harness
func NewAceTeamHarness(baseURL string) *AceTeamHarness {
	return &AceTeamHarness{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// DeviceCodeResponse represents the response from device auth start
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// TokenResponse represents the response from device auth token
type TokenResponse struct {
	Authkey   string `json:"authkey"`
	ExpiresIn int    `json:"expires_in"`
	NexusURL  string `json:"nexus_url,omitempty"`
}

// TokenError represents an error from device auth token
type TokenError struct {
	ErrorCode        string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// StartDeviceAuth initiates the device authorization flow
func (h *AceTeamHarness) StartDeviceAuth(ctx context.Context) (*DeviceCodeResponse, error) {
	url := fmt.Sprintf("%s/api/fabric/device-auth/start", h.baseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result DeviceCodeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// PollForToken polls the token endpoint
func (h *AceTeamHarness) PollForToken(ctx context.Context, deviceCode string) (*TokenResponse, *TokenError, error) {
	url := fmt.Sprintf("%s/api/fabric/device-auth/token", h.baseURL)

	payload := map[string]string{
		"device_code": deviceCode,
		"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
	}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		var result TokenResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, nil, fmt.Errorf("failed to decode response: %w", err)
		}
		return &result, nil, nil
	}

	if resp.StatusCode == http.StatusBadRequest {
		var tokenErr TokenError
		if err := json.Unmarshal(body, &tokenErr); err != nil {
			return nil, nil, fmt.Errorf("failed to decode error: %w", err)
		}
		return nil, &tokenErr, nil
	}

	return nil, nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
}

// ApproveDevice approves a device authorization (requires auth token)
func (h *AceTeamHarness) ApproveDevice(ctx context.Context, userCode, authToken string) error {
	url := fmt.Sprintf("%s/api/fabric/device-auth/approve", h.baseURL)

	payload := map[string]string{
		"user_code": userCode,
	}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", authToken))

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// GetNodes retrieves the list of fabric nodes
func (h *AceTeamHarness) GetNodes(ctx context.Context, authToken string) ([]map[string]interface{}, error) {
	url := fmt.Sprintf("%s/api/fabric/nodes", h.baseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", authToken))

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result []map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return result, nil
}

// HealthCheck checks if the AceTeam API is healthy
func (h *AceTeamHarness) HealthCheck(ctx context.Context) error {
	url := fmt.Sprintf("%s/api/health", h.baseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("health check failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// WaitForReady waits until the AceTeam API is ready
func (h *AceTeamHarness) WaitForReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if err := h.HealthCheck(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	return fmt.Errorf("timeout waiting for AceTeam API to be ready")
}
