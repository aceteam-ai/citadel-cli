// internal/terminal/auth.go
package terminal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// TokenInfo contains the validated token information
type TokenInfo struct {
	// UserID is the user's identifier
	UserID string `json:"user_id"`

	// OrgID is the organization identifier
	OrgID string `json:"org_id"`

	// NodeID is the authorized node identifier (optional)
	NodeID string `json:"node_id,omitempty"`

	// ExpiresAt is when the token expires
	ExpiresAt time.Time `json:"expires_at"`

	// Permissions contains the authorized actions
	Permissions []string `json:"permissions,omitempty"`
}

// TokenValidator defines the interface for validating authentication tokens
type TokenValidator interface {
	// ValidateToken validates a token and returns the token info if valid
	ValidateToken(token string, orgID string) (*TokenInfo, error)
}

// HTTPTokenValidator validates tokens against the AceTeam API
type HTTPTokenValidator struct {
	baseURL    string
	httpClient *http.Client
}

// NewHTTPTokenValidator creates a new HTTP-based token validator
func NewHTTPTokenValidator(baseURL string) *HTTPTokenValidator {
	return &HTTPTokenValidator{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// ValidateToken validates a token against the AceTeam API
func (v *HTTPTokenValidator) ValidateToken(token string, orgID string) (*TokenInfo, error) {
	// Build the validation URL
	url := fmt.Sprintf("%s/api/fabric/terminal/tokens/%s", v.baseURL, orgID)

	// Create the request
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set the authorization header
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	// Execute the request
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, ErrAuthServiceUnavailable
	}
	defer resp.Body.Close()

	// Handle response status codes
	switch resp.StatusCode {
	case http.StatusOK:
		// Token is valid, parse the response
		var tokenInfo TokenInfo
		if err := json.NewDecoder(resp.Body).Decode(&tokenInfo); err != nil {
			return nil, fmt.Errorf("failed to parse token response: %w", err)
		}
		return &tokenInfo, nil

	case http.StatusUnauthorized:
		return nil, ErrInvalidToken

	case http.StatusForbidden:
		return nil, ErrUnauthorized

	case http.StatusNotFound:
		return nil, ErrInvalidToken

	case http.StatusServiceUnavailable:
		return nil, ErrAuthServiceUnavailable

	default:
		return nil, fmt.Errorf("unexpected status code from auth service: %d", resp.StatusCode)
	}
}

// MockTokenValidator is a token validator for testing
type MockTokenValidator struct {
	// ValidTokens maps tokens to their token info
	ValidTokens map[string]*TokenInfo

	// ShouldFail causes all validations to fail
	ShouldFail bool

	// FailError is the error to return when ShouldFail is true
	FailError error
}

// NewMockTokenValidator creates a new mock token validator
func NewMockTokenValidator() *MockTokenValidator {
	return &MockTokenValidator{
		ValidTokens: make(map[string]*TokenInfo),
	}
}

// AddValidToken adds a valid token to the mock validator
func (v *MockTokenValidator) AddValidToken(token string, info *TokenInfo) {
	v.ValidTokens[token] = info
}

// ValidateToken implements TokenValidator for the mock
func (v *MockTokenValidator) ValidateToken(token string, orgID string) (*TokenInfo, error) {
	if v.ShouldFail {
		if v.FailError != nil {
			return nil, v.FailError
		}
		return nil, ErrAuthServiceUnavailable
	}

	info, ok := v.ValidTokens[token]
	if !ok {
		return nil, ErrInvalidToken
	}

	// Check if the orgID matches
	if info.OrgID != orgID {
		return nil, ErrUnauthorized
	}

	// Check if the token has expired
	if !info.ExpiresAt.IsZero() && time.Now().After(info.ExpiresAt) {
		return nil, ErrTokenExpired
	}

	return info, nil
}
