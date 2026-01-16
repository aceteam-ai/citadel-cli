// internal/terminal/auth.go
package terminal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
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

// CachedTokenEntry represents a cached token with its info
type CachedTokenEntry struct {
	Hash      string     // SHA-256 hash of the token
	Info      *TokenInfo // Token metadata
	FetchedAt time.Time  // When this token was fetched
}

// CachingTokenValidator wraps a token validator with local caching
// It fetches token hashes from the API periodically and validates locally
type CachingTokenValidator struct {
	baseURL         string
	orgID           string
	httpClient      *http.Client
	cache           map[string]*CachedTokenEntry // SHA-256 hash -> entry
	cacheMu         sync.RWMutex
	refreshInterval time.Duration
	lastRefresh     time.Time
	stopCh          chan struct{}
	backoff         time.Duration
	minBackoff      time.Duration
	maxBackoff      time.Duration
}

// NewCachingTokenValidator creates a new caching token validator
func NewCachingTokenValidator(baseURL, orgID string, refreshInterval time.Duration) *CachingTokenValidator {
	return &CachingTokenValidator{
		baseURL:         baseURL,
		orgID:           orgID,
		httpClient:      &http.Client{Timeout: 10 * time.Second},
		cache:           make(map[string]*CachedTokenEntry),
		refreshInterval: refreshInterval,
		stopCh:          make(chan struct{}),
		minBackoff:      1 * time.Second,
		maxBackoff:      5 * time.Minute,
		backoff:         1 * time.Second,
	}
}

// Start begins the background token refresh goroutine
func (v *CachingTokenValidator) Start() error {
	// Initial fetch
	if err := v.fetchAndCacheTokens(); err != nil {
		// Log but don't fail - we'll retry on the schedule
		fmt.Printf("Warning: initial token fetch failed: %v\n", err)
	}

	// Start background refresh
	go v.refreshLoop()
	return nil
}

// Stop stops the background refresh goroutine
func (v *CachingTokenValidator) Stop() {
	close(v.stopCh)
}

// refreshLoop runs the periodic token refresh
func (v *CachingTokenValidator) refreshLoop() {
	ticker := time.NewTicker(v.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-v.stopCh:
			return
		case <-ticker.C:
			if err := v.fetchAndCacheTokens(); err != nil {
				// Use exponential backoff on failures
				v.backoff = min(v.backoff*2, v.maxBackoff)
				fmt.Printf("Warning: token refresh failed (will retry in %v): %v\n", v.backoff, err)
			} else {
				// Reset backoff on success
				v.backoff = v.minBackoff
			}
		}
	}
}

// TokensResponse represents the response from the token list API
type TokensResponse struct {
	Tokens []TokenHashEntry `json:"tokens"`
}

// TokenHashEntry represents a single token entry from the API
type TokenHashEntry struct {
	Hash      string    `json:"hash"`
	UserID    string    `json:"user_id"`
	OrgID     string    `json:"org_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// fetchAndCacheTokens fetches token hashes from the API and updates the cache
func (v *CachingTokenValidator) fetchAndCacheTokens() error {
	url := fmt.Sprintf("%s/api/fabric/terminal/tokens/%s", v.baseURL, v.orgID)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch tokens: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token API returned status %d", resp.StatusCode)
	}

	var tokensResp TokensResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokensResp); err != nil {
		return fmt.Errorf("failed to parse token response: %w", err)
	}

	// Update cache
	v.cacheMu.Lock()
	defer v.cacheMu.Unlock()

	// Clear old cache and populate with new tokens
	v.cache = make(map[string]*CachedTokenEntry)
	now := time.Now()

	for _, entry := range tokensResp.Tokens {
		v.cache[entry.Hash] = &CachedTokenEntry{
			Hash: entry.Hash,
			Info: &TokenInfo{
				UserID:    entry.UserID,
				OrgID:     entry.OrgID,
				ExpiresAt: entry.ExpiresAt,
			},
			FetchedAt: now,
		}
	}

	v.lastRefresh = now
	return nil
}

// ValidateToken validates a token using the local cache
func (v *CachingTokenValidator) ValidateToken(token string, orgID string) (*TokenInfo, error) {
	// Hash the incoming token
	hash := hashToken(token)

	// Check cache
	v.cacheMu.RLock()
	entry, ok := v.cache[hash]
	v.cacheMu.RUnlock()

	if !ok {
		// Token not found in cache - try a refresh and check again
		if err := v.fetchAndCacheTokens(); err == nil {
			v.cacheMu.RLock()
			entry, ok = v.cache[hash]
			v.cacheMu.RUnlock()
		}

		if !ok {
			return nil, ErrInvalidToken
		}
	}

	// Verify org ID matches
	if entry.Info.OrgID != orgID {
		return nil, ErrUnauthorized
	}

	// Check expiration
	if !entry.Info.ExpiresAt.IsZero() && time.Now().After(entry.Info.ExpiresAt) {
		return nil, ErrTokenExpired
	}

	return entry.Info, nil
}

// hashToken computes the SHA-256 hash of a token
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// CacheSize returns the number of tokens in the cache (for testing)
func (v *CachingTokenValidator) CacheSize() int {
	v.cacheMu.RLock()
	defer v.cacheMu.RUnlock()
	return len(v.cache)
}

// LastRefreshTime returns when the cache was last refreshed (for testing)
func (v *CachingTokenValidator) LastRefreshTime() time.Time {
	v.cacheMu.RLock()
	defer v.cacheMu.RUnlock()
	return v.lastRefresh
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
