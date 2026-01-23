// internal/nexus/deviceauth.go
package nexus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// DeviceAuthClient handles OAuth 2.0 Device Authorization Grant flow (RFC 8628)
type DeviceAuthClient struct {
	baseURL    string
	httpClient *http.Client
}

// DeviceCodeResponse represents the response from the /start endpoint
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// TokenResponse represents a successful token response
type TokenResponse struct {
	Authkey        string `json:"authkey"`
	ExpiresIn      int    `json:"expires_in"`
	NexusURL       string `json:"nexus_url,omitempty"`
	OrgID          string `json:"org_id,omitempty"`
	RedisURL       string `json:"redis_url,omitempty"`       // Deprecated: use DeviceAPIToken
	DeviceAPIToken string `json:"device_api_token,omitempty"` // New secure API token
	APIBaseURL     string `json:"api_base_url,omitempty"`     // Base URL for API calls
}

// TokenError represents an error response from the /token endpoint
type TokenError struct {
	ErrorCode        string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
	Interval         int    `json:"interval,omitempty"` // For slow_down error
}

// StartFlowRequest represents the request body for /start endpoint
type StartFlowRequest struct {
	ClientID      string `json:"client_id"`
	ClientVersion string `json:"client_version"`
	Hostname      string `json:"hostname,omitempty"`
	MachineID     string `json:"machine_id,omitempty"`
	ForceNew      bool   `json:"force_new,omitempty"`
}

// StartFlowOptions contains options for starting the device authorization flow
type StartFlowOptions struct {
	ForceNew bool // Force fresh registration, ignoring existing machine mapping
}

// TokenRequest represents the request body for /token endpoint
type TokenRequest struct {
	DeviceCode string `json:"device_code"`
	GrantType  string `json:"grant_type"`
}

// NewDeviceAuthClient creates a new device authorization client
func NewDeviceAuthClient(baseURL string) *DeviceAuthClient {
	return &DeviceAuthClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// StartFlow initiates the device authorization flow by requesting device and user codes.
// If opts is nil, default options are used.
func (c *DeviceAuthClient) StartFlow(opts *StartFlowOptions) (*DeviceCodeResponse, error) {
	url := c.baseURL + "/api/fabric/device-auth/start"

	// Get hostname for device identification
	hostname, _ := os.Hostname()

	// Generate machine ID for device fingerprinting
	machineID, _ := platform.GenerateMachineID()

	// Create request body
	reqBody := StartFlowRequest{
		ClientID:      "citadel-cli",
		ClientVersion: "1.0.0", // TODO: Get from version const
		Hostname:      hostname,
		MachineID:     machineID,
	}

	// Apply options if provided
	if opts != nil {
		reqBody.ForceNew = opts.ForceNew
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Execute request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to authentication service: %w", err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil, fmt.Errorf("authentication service is temporarily unavailable")
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("rate limit exceeded, please try again in a few minutes")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authentication service returned status %d", resp.StatusCode)
	}

	// Parse response
	var response DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Override verification URI to use the auth service base URL
	// This ensures local development works correctly
	response.VerificationURI = c.baseURL + "/device"
	response.VerificationURIComplete = c.baseURL + "/device?code=" + response.UserCode

	return &response, nil
}

// PollForToken polls the /token endpoint until authorization is complete or timeout occurs
func (c *DeviceAuthClient) PollForToken(deviceCode string, interval int) (*TokenResponse, error) {
	pollingInterval := time.Duration(interval) * time.Second
	timeout := 10 * time.Minute // Match backend expiration
	startTime := time.Now()

	for time.Since(startTime) < timeout {
		// Make token request
		token, err := c.checkToken(deviceCode)

		// Success case
		if token != nil && token.Authkey != "" {
			return token, nil
		}

		// Handle errors
		if err != nil {
			tokenErr, ok := err.(*TokenError)
			if !ok {
				// Network or HTTP error
				return nil, fmt.Errorf("token request failed: %w", err)
			}

			// Handle RFC 8628 error codes
			switch tokenErr.ErrorCode {
			case "authorization_pending":
				// Keep polling, do nothing
			case "slow_down":
				// Increase interval by 5 seconds
				pollingInterval += 5 * time.Second
			case "expired_token":
				return nil, fmt.Errorf("device code expired after 10 minutes, please run the command again")
			case "access_denied":
				return nil, fmt.Errorf("authorization denied by user")
			default:
				return nil, fmt.Errorf("authentication error: %s", tokenErr.ErrorDescription)
			}
		}

		// Wait before next poll
		time.Sleep(pollingInterval)
	}

	return nil, fmt.Errorf("authentication timeout after 10 minutes")
}

// checkToken makes a single request to the /token endpoint
func (c *DeviceAuthClient) checkToken(deviceCode string) (*TokenResponse, error) {
	url := c.baseURL + "/api/fabric/device-auth/token"

	// Create request body
	reqBody := TokenRequest{
		DeviceCode: deviceCode,
		GrantType:  "urn:ietf:params:oauth:grant-type:device_code",
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Execute request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Success case
	if resp.StatusCode == http.StatusOK {
		var tokenResp TokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
			return nil, fmt.Errorf("failed to parse token response: %w", err)
		}
		return &tokenResp, nil
	}

	// Error case - parse error response
	if resp.StatusCode == http.StatusBadRequest {
		var tokenErr TokenError
		if err := json.NewDecoder(resp.Body).Decode(&tokenErr); err != nil {
			return nil, fmt.Errorf("failed to parse error response: %w", err)
		}
		return nil, &tokenErr
	}

	// Other HTTP errors
	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil, fmt.Errorf("authentication service unavailable")
	}

	return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
}

// Error implements the error interface for TokenError
func (e *TokenError) Error() string {
	if e.ErrorDescription != "" {
		return fmt.Sprintf("%s: %s", e.ErrorCode, e.ErrorDescription)
	}
	return e.ErrorCode
}
