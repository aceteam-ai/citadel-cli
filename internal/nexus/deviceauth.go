// internal/nexus/deviceauth.go
package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// ErrAPIUnreachable is returned when the AceTeam API cannot be reached.
// Callers should check network connectivity and retry.
var ErrAPIUnreachable = errors.New("cannot reach AceTeam API")

// ErrTokenExpired is returned when the device API token has been revoked or expired.
var ErrTokenExpired = errors.New("device API token expired or revoked")

// CheckAPIReachable performs a fast connectivity check against the AceTeam API.
// Returns nil if the API responds within the timeout, or a descriptive error
// explaining why it cannot be reached (DNS failure, connection refused, timeout).
func CheckAPIReachable(baseURL string) error {
	if baseURL == "" {
		return fmt.Errorf("%w: no API URL configured", ErrAPIUnreachable)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url := baseURL + "/api/fabric/device-auth/start"
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAPIUnreachable, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return classifyNetworkError(err, baseURL)
	}
	defer resp.Body.Close()

	// Any HTTP response (even 405 Method Not Allowed) means the server is reachable
	return nil
}

// classifyNetworkError turns a raw HTTP client error into a user-friendly
// message that distinguishes DNS failures, connection refused, and timeouts.
func classifyNetworkError(err error, baseURL string) error {
	if err == nil {
		return nil
	}

	msg := err.Error()

	// DNS resolution failure
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Errorf("%w: DNS lookup failed for %s — check your internet connection", ErrAPIUnreachable, baseURL)
	}

	// Connection refused (server down or wrong port)
	if strings.Contains(msg, "connection refused") {
		return fmt.Errorf("%w: connection refused at %s — the API server may be down", ErrAPIUnreachable, baseURL)
	}

	// Timeout
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return fmt.Errorf("%w: connection timed out reaching %s — check your network", ErrAPIUnreachable, baseURL)
	}

	// TLS errors
	if strings.Contains(msg, "certificate") || strings.Contains(msg, "tls") || strings.Contains(msg, "x509") {
		return fmt.Errorf("%w: TLS error connecting to %s — %v", ErrAPIUnreachable, baseURL, err)
	}

	// Catch-all
	return fmt.Errorf("%w: %v", ErrAPIUnreachable, err)
}

// IsNetworkError returns true if the error indicates a network connectivity
// problem (as opposed to an authentication or server-side error).
func IsNetworkError(err error) bool {
	return errors.Is(err, ErrAPIUnreachable)
}

// IsAuthError returns true if the error indicates an authentication failure
// (expired token, revoked access, etc).
func IsAuthError(err error) bool {
	return errors.Is(err, ErrTokenExpired)
}

// ClassifyHTTPError maps an HTTP status code and response body to a
// descriptive error with appropriate sentinel wrapping.
func ClassifyHTTPError(statusCode int, body string) error {
	switch statusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: server returned 401 Unauthorized — re-run 'citadel init' to re-authenticate", ErrTokenExpired)
	case http.StatusForbidden:
		return fmt.Errorf("%w: server returned 403 Forbidden — your token may have been revoked", ErrTokenExpired)
	case http.StatusServiceUnavailable:
		return fmt.Errorf("service temporarily unavailable (503) — try again shortly")
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return fmt.Errorf("API gateway error (%d) — the service may be restarting", statusCode)
	default:
		if body != "" {
			return fmt.Errorf("API returned HTTP %d: %s", statusCode, body)
		}
		return fmt.Errorf("API returned HTTP %d", statusCode)
	}
}

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
	OrgName        string `json:"org_name,omitempty"`         // Human-readable org name
	RedisURL       string `json:"redis_url,omitempty"`        // Deprecated: use DeviceAPIToken
	DeviceAPIToken string `json:"device_api_token,omitempty"` // New secure API token
	APIBaseURL     string `json:"api_base_url,omitempty"`     // Base URL for API calls
	UserEmail      string `json:"user_email,omitempty"`       // User email for display
	UserName       string `json:"user_name,omitempty"`        // User display name
	// FabricNodeID is the numeric AceTeam fabric/platform node ID (aceteam
	// #8139). NOT sent by the backend today -- this is the citadel-side hook
	// for one of two candidate echo points the design doc leaves open (the
	// other is a heartbeat ack); see
	// docs/design-node-identity-receipts.md §2/§4. Additive and inert until
	// the backend starts populating it: an empty value here is the expected,
	// universal case, not an error.
	FabricNodeID string `json:"fabric_node_id,omitempty"`
	// LeafPem / ChainPem / NodeUID are the CSR-enrollment bundle a device-grant
	// login receives when it submits a CSR (citadel-cli#1062, companion to the
	// draft platform aceteam#9576). LeafPem is the fabric CA leaf bound to the
	// submitted CSR's public key; ChainPem is the CA trust chain; NodeUID is the
	// server-assigned fabric node id used to derive the deterministic serving
	// identity ("node-"+NodeUID). All THREE are additive and inert until the
	// backend starts populating them: an empty value is the expected, universal
	// case today (a legacy backend or an un-activated CA returns none), NOT an
	// error. These mirror internal/devicemode's pairing bundle keys.
	LeafPem  string `json:"leaf_pem,omitempty"`
	ChainPem string `json:"chain_pem,omitempty"`
	NodeUID  string `json:"node_uid,omitempty"`
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
	// DeviceKind selects the backend authorization path. Absent (empty) reads
	// as "citadel" server-side, so existing callers are unaffected. The memory
	// onboarding installer (aceteam #7160) sends "memory" so the backend mints
	// a scoped act_ API key instead of a Headscale preauthkey.
	DeviceKind string `json:"device_kind,omitempty"`
}

// StartFlowOptions contains options for starting the device authorization flow
type StartFlowOptions struct {
	ForceNew bool // Force fresh registration, ignoring existing machine mapping
	// DeviceKind, when set (e.g. "memory"), is forwarded to the /start endpoint
	// to select a non-citadel authorization path. Empty = citadel (default).
	DeviceKind string
}

// MemoryTokenResponse is the poll response for a device_kind:"memory" flow
// (aceteam #7160). Unlike the citadel TokenResponse (which returns a Headscale
// authkey), a live memory record returns HTTP 200 and signals pending/approved
// via Status. The shared endpoint still returns RFC 8628 HTTP 400 errors after
// expiry or denial; checkMemoryTokenContext maps those into the same lifecycle
// statuses. On approval it carries a scoped act_ API key in APIKey — the same
// credential external MCP clients (like Claude Code) authenticate with.
type MemoryTokenResponse struct {
	Status    string   `json:"status"` // pending | approved | expired | denied
	APIKey    string   `json:"api_key,omitempty"`
	ExpiresIn *int     `json:"expires_in,omitempty"` // nullable; memory keys are minted without expiry
	OrgID     string   `json:"org_id,omitempty"`
	OrgName   string   `json:"org_name,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
}

// TokenRequest represents the request body for /token endpoint
type TokenRequest struct {
	DeviceCode string `json:"device_code"`
	GrantType  string `json:"grant_type"`
	// CSRPem, when non-empty, is a PEM PKCS#10 certificate signing request the
	// backend may sign into a fabric CA leaf (citadel-cli#1062). Only interactive
	// device-grant `citadel login` sends it; every legacy caller leaves it empty,
	// and `omitempty` keeps their request bodies byte-identical (the key is
	// absent, not present-and-empty). NEVER logged.
	CSRPem string `json:"csr_pem,omitempty"`
}

// NewDeviceAuthClient creates a new device authorization client
func NewDeviceAuthClient(baseURL string) *DeviceAuthClient {
	return &DeviceAuthClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
			// Device codes authorize credential minting and must not be replayed
			// to a redirect target. Endpoint changes must be explicit.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
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
		reqBody.DeviceKind = opts.DeviceKind
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
		return nil, classifyNetworkError(err, c.baseURL)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil, fmt.Errorf("authentication service is temporarily unavailable (503)")
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

// PollForToken polls the /token endpoint until authorization is complete or
// timeout occurs. Legacy no-CSR path: delegates to PollForTokenWithCSR with an
// empty CSR so the request body stays byte-identical to before #1062.
func (c *DeviceAuthClient) PollForToken(deviceCode string, interval int) (*TokenResponse, error) {
	return c.PollForTokenWithCSR(deviceCode, interval, "")
}

// PollForTokenWithCSR is PollForToken plus a CSR (citadel-cli#1062). The SAME
// csrPEM is sent on EVERY poll retry (it is captured once by the caller, before
// polling begins), so an approved grant signs the identity key the caller
// already committed to. An empty csrPEM reproduces the legacy no-CSR request
// exactly. RFC 8628 pending/slow_down/expired/denied semantics are unchanged.
func (c *DeviceAuthClient) PollForTokenWithCSR(deviceCode string, interval int, csrPEM string) (*TokenResponse, error) {
	pollingInterval := time.Duration(interval) * time.Second
	timeout := 10 * time.Minute // Match backend expiration
	startTime := time.Now()

	// csr may be dropped mid-flight. A certificate-enrollment failure on a
	// CSR-bearing poll (citadel-cli#1062) must NOT abort login: the device grant
	// is still approved, so we clear the CSR and keep polling, and the next
	// no-CSR poll delivers the authkey. The node stays honestly unverified
	// instead of failing to log in over a transient CA hiccup or rate limit.
	csr := csrPEM

	for time.Since(startTime) < timeout {
		// Make token request (same CSR on every retry, until it is dropped).
		token, err := c.CheckTokenWithCSR(deviceCode, csr)

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
				// Not a standard RFC 8628 code. If it is a certificate
				// enrollment failure and we still carry a CSR, drop the CSR and
				// keep polling (citadel-cli#1062) rather than abort login.
				if csr != "" && isCertificateEnrollmentCode(tokenErr.ErrorCode) {
					csr = ""
					break
				}
				return nil, fmt.Errorf("authentication error: %s", tokenErr.ErrorDescription)
			}
		}

		// Wait before next poll
		time.Sleep(pollingInterval)
	}

	return nil, fmt.Errorf("authentication timeout after 10 minutes")
}

// certificateEnrollmentErrorCodes are the error codes the device-auth
// certificate-enrollment path (citadel-cli#1062, companion to aceteam#9576)
// returns when leaf issuance fails but the device grant itself is still valid.
// They are reachable only on a CSR-bearing poll. Encountering one means "drop
// the CSR and keep polling": a subsequent no-CSR poll still delivers the
// authkey, so login proceeds with the node honestly unverified.
var certificateEnrollmentErrorCodes = map[string]bool{
	"certificate_enrollment_unavailable":  true,
	"certificate_enrollment_rate_limited": true,
	"certificate_enrollment_failed":       true,
	"unsupported_device_kind":             true,
}

// isCertificateEnrollmentCode reports whether code is one of the
// certificate-enrollment failure codes above (never a standard RFC 8628 code).
func isCertificateEnrollmentCode(code string) bool {
	return certificateEnrollmentErrorCodes[code]
}

// decodeErrorCode best-effort decodes an {"error": "<code>"} body and returns
// the code, or "" when the body is not that shape.
func decodeErrorCode(r io.Reader) string {
	var e TokenError
	if json.NewDecoder(r).Decode(&e) != nil {
		return ""
	}
	return e.ErrorCode
}

// CheckToken makes a single request to the /token endpoint.
// This is useful for non-blocking polling in UIs. Legacy no-CSR path.
func (c *DeviceAuthClient) CheckToken(deviceCode string) (*TokenResponse, error) {
	return c.CheckTokenWithCSR(deviceCode, "")
}

// CheckTokenWithCSR is CheckToken plus an optional PEM CSR (citadel-cli#1062).
// An empty csrPEM omits the csr_pem field entirely, so the request body is
// byte-identical to the legacy CheckToken request.
func (c *DeviceAuthClient) CheckTokenWithCSR(deviceCode, csrPEM string) (*TokenResponse, error) {
	url := c.baseURL + "/api/fabric/device-auth/token"

	// Create request body
	reqBody := TokenRequest{
		DeviceCode: deviceCode,
		GrantType:  "urn:ietf:params:oauth:grant-type:device_code",
		CSRPem:     csrPEM,
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
		return nil, classifyNetworkError(err, c.baseURL)
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

	// The certificate-enrollment path (citadel-cli#1062, reachable only when a
	// CSR was submitted) returns {"error": "<code>"} with 409/429/503 when leaf
	// issuance fails while the device grant is still valid. Surface a recognized
	// enrollment code as a *TokenError so PollForTokenWithCSR can drop the CSR
	// and keep polling. Any other body keeps the legacy generic error, so the
	// no-CSR paths are byte-for-byte unchanged.
	if code := decodeErrorCode(resp.Body); isCertificateEnrollmentCode(code) {
		return nil, &TokenError{ErrorCode: code}
	}

	// Other HTTP errors
	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil, fmt.Errorf("authentication service unavailable")
	}

	return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
}

// PollForMemoryToken polls the /token endpoint for a device_kind:"memory" flow
// until the device is approved, denied, expires, or the client times out.
//
// Live memory records return HTTP 200 with a Status field (they do NOT use the
// RFC 8628 authorization_pending error), so this method cannot reuse
// PollForToken. Shared-endpoint expiry/denial errors are normalized separately.
// The citadel PollForToken path is left untouched.
func (c *DeviceAuthClient) PollForMemoryToken(deviceCode string, interval int) (*MemoryTokenResponse, error) {
	return c.PollForMemoryTokenContext(context.Background(), deviceCode, interval)
}

const (
	defaultMemoryPollInterval = 5 * time.Second
	maxMemoryPollInterval     = 30 * time.Second
	memoryPollTimeout         = 10 * time.Minute
)

// PollForMemoryTokenContext is PollForMemoryToken with caller cancellation.
// The backend-provided interval is normalized to a finite safe range, and the
// timer is always capped to the remaining authorization budget.
func (c *DeviceAuthClient) PollForMemoryTokenContext(ctx context.Context, deviceCode string, interval int) (*MemoryTokenResponse, error) {
	pollingInterval := normalizeMemoryPollInterval(interval)
	return c.pollMemoryContext(ctx, deviceCode, pollingInterval, memoryPollTimeout)
}

func normalizeMemoryPollInterval(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultMemoryPollInterval
	}
	if seconds > int(maxMemoryPollInterval/time.Second) {
		return maxMemoryPollInterval
	}
	return time.Duration(seconds) * time.Second
}

// pollMemory is retained as the duration-parameterized test seam.
func (c *DeviceAuthClient) pollMemory(deviceCode string, pollingInterval, timeout time.Duration) (*MemoryTokenResponse, error) {
	return c.pollMemoryContext(context.Background(), deviceCode, pollingInterval, timeout)
}

func (c *DeviceAuthClient) pollMemoryContext(parent context.Context, deviceCode string, pollingInterval, timeout time.Duration) (*MemoryTokenResponse, error) {
	if pollingInterval <= 0 {
		return nil, fmt.Errorf("memory polling interval must be positive")
	}
	if pollingInterval > maxMemoryPollInterval {
		pollingInterval = maxMemoryPollInterval
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	for {
		resp, err := c.checkMemoryTokenContext(ctx, deviceCode)
		if err != nil {
			// classifyNetworkError intentionally presents friendly errors and does
			// not retain the request error in its chain, so consult the governing
			// context as well when the request ended at our poll deadline.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("authentication timeout after %s", timeout)
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}

		switch resp.Status {
		case "approved":
			if resp.APIKey == "" {
				return nil, fmt.Errorf("device approved but no API key was returned")
			}
			return resp, nil
		case "denied":
			return nil, fmt.Errorf("authorization denied by user")
		case "expired":
			return nil, fmt.Errorf("device code expired, please run the command again")
		case "pending":
			// Keep polling within the context deadline below.
		default:
			return nil, fmt.Errorf("unexpected memory authorization status %q", resp.Status)
		}

		wait := pollingInterval
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nil, fmt.Errorf("authentication timeout after %s", timeout)
			}
			if wait > remaining {
				wait = remaining
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("authentication timeout after %s", timeout)
			}
			return nil, ctx.Err()
		}
	}
}

// checkMemoryToken makes a single /token request for a memory-kind device.
func (c *DeviceAuthClient) checkMemoryToken(deviceCode string) (*MemoryTokenResponse, error) {
	return c.checkMemoryTokenContext(context.Background(), deviceCode)
}

func (c *DeviceAuthClient) checkMemoryTokenContext(ctx context.Context, deviceCode string) (*MemoryTokenResponse, error) {
	url := c.baseURL + "/api/fabric/device-auth/token"

	reqBody := TokenRequest{
		DeviceCode: deviceCode,
		GrantType:  "urn:ietf:params:oauth:grant-type:device_code",
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, classifyNetworkError(err, c.baseURL)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil, fmt.Errorf("authentication service unavailable")
	}
	if resp.StatusCode == http.StatusBadRequest {
		var tokenErr TokenError
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&tokenErr); err != nil {
			return nil, fmt.Errorf("failed to parse memory token error response: %w", err)
		}
		switch tokenErr.ErrorCode {
		case "expired_token":
			return &MemoryTokenResponse{Status: "expired"}, nil
		case "access_denied":
			return &MemoryTokenResponse{Status: "denied"}, nil
		case "authorization_pending":
			return &MemoryTokenResponse{Status: "pending"}, nil
		default:
			return nil, fmt.Errorf("memory token request failed: %s", tokenErr.Error())
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var out MemoryTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}
	return &out, nil
}

// Error implements the error interface for TokenError
func (e *TokenError) Error() string {
	if e.ErrorDescription != "" {
		return fmt.Sprintf("%s: %s", e.ErrorCode, e.ErrorDescription)
	}
	return e.ErrorCode
}
