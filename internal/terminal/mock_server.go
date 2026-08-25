// internal/terminal/mock_server.go
package terminal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// MockAuthServer provides a mock HTTP server for testing token validation
type MockAuthServer struct {
	server *httptest.Server

	mu          sync.RWMutex
	validTokens map[string]*TokenInfo
	// previousGrace maps a current token to the (hash, expiry) of the token it
	// rotated out, for exercising the rotation-grace-window hashes response.
	previousGrace map[string]previousGraceEntry

	// RequestCount tracks the number of validation requests
	RequestCount int

	// ShouldFail causes all requests to return an error
	ShouldFail bool

	// FailStatusCode is the HTTP status code to return when failing
	FailStatusCode int
}

// previousGraceEntry is the hash + expiry of a token that was rotated out but
// is still accepted during the grace window (see TokenHashEntry.PreviousHash).
type previousGraceEntry struct {
	Hash      string
	ExpiresAt time.Time
}

// StartMockAuthServer creates and starts a mock auth server
func StartMockAuthServer() *MockAuthServer {
	mock := &MockAuthServer{
		validTokens:    make(map[string]*TokenInfo),
		previousGrace:  make(map[string]previousGraceEntry),
		FailStatusCode: http.StatusServiceUnavailable,
	}

	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mock.mu.Lock()
		mock.RequestCount++
		shouldFail := mock.ShouldFail
		failStatus := mock.FailStatusCode
		mock.mu.Unlock()

		if shouldFail {
			http.Error(w, "service unavailable", failStatus)
			return
		}

		// Expect path like /api/fabric/terminal/tokens/{orgId}
		if !strings.HasPrefix(r.URL.Path, "/api/fabric/terminal/tokens/") {
			http.NotFound(w, r)
			return
		}

		// Extract org ID from path
		pathParts := strings.Split(r.URL.Path, "/")
		if len(pathParts) < 6 {
			http.NotFound(w, r)
			return
		}
		orgID := pathParts[5]

		// Check for Authorization header
		authHeader := r.Header.Get("Authorization")

		// If no Authorization header, return list of token hashes (for CachingTokenValidator)
		if authHeader == "" {
			mock.mu.RLock()
			var tokens []TokenHashEntry
			for token, info := range mock.validTokens {
				if info.OrgID == orgID {
					h := sha256.Sum256([]byte(token))
					entry := TokenHashEntry{
						Hash:      hex.EncodeToString(h[:]),
						UserID:    info.UserID,
						OrgID:     info.OrgID,
						ExpiresAt: info.ExpiresAt,
					}
					if grace, ok := mock.previousGrace[token]; ok {
						entry.PreviousHash = grace.Hash
						entry.PreviousHashExpiresAt = grace.ExpiresAt
					}
					tokens = append(tokens, entry)
				}
			}
			mock.mu.RUnlock()

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(TokensResponse{Tokens: tokens})
			return
		}

		// Single token validation path (for HTTPTokenValidator)
		if !strings.HasPrefix(authHeader, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(authHeader, "Bearer ")

		// Look up the token
		mock.mu.RLock()
		tokenInfo, ok := mock.validTokens[token]
		mock.mu.RUnlock()

		if !ok {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		// Check org ID matches
		if tokenInfo.OrgID != orgID {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// Check expiration
		if !tokenInfo.ExpiresAt.IsZero() && time.Now().After(tokenInfo.ExpiresAt) {
			http.Error(w, "token expired", http.StatusUnauthorized)
			return
		}

		// Return the token info
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenInfo)
	}))

	return mock
}

// URL returns the base URL of the mock server
func (m *MockAuthServer) URL() string {
	return m.server.URL
}

// Close shuts down the mock server
func (m *MockAuthServer) Close() {
	m.server.Close()
}

// AddValidToken adds a valid token to the mock server
func (m *MockAuthServer) AddValidToken(token string, info *TokenInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.validTokens[token] = info
}

// SetPreviousToken records that currentToken's PREVIOUS token (the one it
// rotated out of) was previousToken, accepted until expiresAt. This exercises
// the rotation-grace-window fields on the /hashes response.
func (m *MockAuthServer) SetPreviousToken(currentToken, previousToken string, expiresAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := sha256.Sum256([]byte(previousToken))
	m.previousGrace[currentToken] = previousGraceEntry{
		Hash:      hex.EncodeToString(h[:]),
		ExpiresAt: expiresAt,
	}
}

// RemoveToken removes a token from the mock server
func (m *MockAuthServer) RemoveToken(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.validTokens, token)
}

// SetShouldFail configures whether the server should fail all requests
func (m *MockAuthServer) SetShouldFail(fail bool, statusCode int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ShouldFail = fail
	if statusCode != 0 {
		m.FailStatusCode = statusCode
	}
}

// GetRequestCount returns the number of requests made
func (m *MockAuthServer) GetRequestCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.RequestCount
}

// ResetRequestCount resets the request counter
func (m *MockAuthServer) ResetRequestCount() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RequestCount = 0
}

// Clear removes all valid tokens
func (m *MockAuthServer) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.validTokens = make(map[string]*TokenInfo)
	m.previousGrace = make(map[string]previousGraceEntry)
	m.RequestCount = 0
	m.ShouldFail = false
}
