// internal/terminal/auth_test.go
package terminal

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPTokenValidator(t *testing.T) {
	// Start mock auth server
	mock := StartMockAuthServer()
	defer mock.Close()

	// Add a valid token
	validToken := "valid-test-token-12345"
	tokenInfo := &TokenInfo{
		UserID:    "user-123",
		OrgID:     "org-456",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	mock.AddValidToken(validToken, tokenInfo)

	// Create validator
	validator := NewHTTPTokenValidator(mock.URL())

	t.Run("valid token", func(t *testing.T) {
		info, err := validator.ValidateToken(validToken, "org-456")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if info.UserID != "user-123" {
			t.Errorf("expected UserID user-123, got %s", info.UserID)
		}
		if info.OrgID != "org-456" {
			t.Errorf("expected OrgID org-456, got %s", info.OrgID)
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		_, err := validator.ValidateToken("invalid-token", "org-456")
		if err != ErrInvalidToken {
			t.Errorf("expected ErrInvalidToken, got %v", err)
		}
	})

	t.Run("wrong org ID", func(t *testing.T) {
		_, err := validator.ValidateToken(validToken, "wrong-org")
		if err != ErrUnauthorized {
			t.Errorf("expected ErrUnauthorized, got %v", err)
		}
	})

	t.Run("service unavailable", func(t *testing.T) {
		mock.SetShouldFail(true, 503)
		_, err := validator.ValidateToken(validToken, "org-456")
		if err == nil {
			t.Error("expected error when service is unavailable")
		}
		mock.SetShouldFail(false, 0)
	})
}

func TestHTTPTokenValidatorExpiredToken(t *testing.T) {
	mock := StartMockAuthServer()
	defer mock.Close()

	// Add an expired token
	expiredToken := "expired-token-12345"
	tokenInfo := &TokenInfo{
		UserID:    "user-123",
		OrgID:     "org-456",
		ExpiresAt: time.Now().Add(-time.Hour), // Expired
	}
	mock.AddValidToken(expiredToken, tokenInfo)

	validator := NewHTTPTokenValidator(mock.URL())

	_, err := validator.ValidateToken(expiredToken, "org-456")
	if err != ErrInvalidToken {
		t.Errorf("expected ErrInvalidToken for expired token, got %v", err)
	}
}

func TestMockTokenValidator(t *testing.T) {
	validator := NewMockTokenValidator()

	// Add a valid token
	validToken := "mock-valid-token"
	tokenInfo := &TokenInfo{
		UserID:    "mock-user",
		OrgID:     "mock-org",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	validator.AddValidToken(validToken, tokenInfo)

	t.Run("valid token", func(t *testing.T) {
		info, err := validator.ValidateToken(validToken, "mock-org")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if info.UserID != "mock-user" {
			t.Errorf("expected UserID mock-user, got %s", info.UserID)
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		_, err := validator.ValidateToken("invalid", "mock-org")
		if err != ErrInvalidToken {
			t.Errorf("expected ErrInvalidToken, got %v", err)
		}
	})

	t.Run("wrong org", func(t *testing.T) {
		_, err := validator.ValidateToken(validToken, "wrong-org")
		if err != ErrUnauthorized {
			t.Errorf("expected ErrUnauthorized, got %v", err)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		expiredToken := "expired-mock-token"
		validator.AddValidToken(expiredToken, &TokenInfo{
			UserID:    "user",
			OrgID:     "mock-org",
			ExpiresAt: time.Now().Add(-time.Hour),
		})
		_, err := validator.ValidateToken(expiredToken, "mock-org")
		if err != ErrTokenExpired {
			t.Errorf("expected ErrTokenExpired, got %v", err)
		}
	})

	t.Run("forced failure", func(t *testing.T) {
		validator.ShouldFail = true
		validator.FailError = ErrAuthServiceUnavailable
		_, err := validator.ValidateToken(validToken, "mock-org")
		if err != ErrAuthServiceUnavailable {
			t.Errorf("expected ErrAuthServiceUnavailable, got %v", err)
		}
		validator.ShouldFail = false
	})
}

func TestMockAuthServer(t *testing.T) {
	mock := StartMockAuthServer()
	defer mock.Close()

	t.Run("URL is set", func(t *testing.T) {
		if mock.URL() == "" {
			t.Error("expected URL to be set")
		}
	})

	t.Run("request count", func(t *testing.T) {
		mock.ResetRequestCount()
		if mock.GetRequestCount() != 0 {
			t.Errorf("expected request count 0, got %d", mock.GetRequestCount())
		}

		// Make a request
		validator := NewHTTPTokenValidator(mock.URL())
		validator.ValidateToken("any-token", "any-org")

		if mock.GetRequestCount() != 1 {
			t.Errorf("expected request count 1, got %d", mock.GetRequestCount())
		}
	})

	t.Run("clear", func(t *testing.T) {
		mock.AddValidToken("token", &TokenInfo{OrgID: "org"})
		mock.Clear()

		validator := NewHTTPTokenValidator(mock.URL())
		_, err := validator.ValidateToken("token", "org")
		if err != ErrInvalidToken {
			t.Errorf("expected ErrInvalidToken after clear, got %v", err)
		}
	})
}

func TestCachingTokenValidator(t *testing.T) {
	mock := StartMockAuthServer()
	defer mock.Close()

	// Add valid tokens
	validToken := "cached-test-token-12345"
	tokenInfo := &TokenInfo{
		UserID:    "user-123",
		OrgID:     "org-456",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	mock.AddValidToken(validToken, tokenInfo)

	// Create caching validator
	validator := NewCachingTokenValidator(mock.URL(), "org-456", "", time.Hour)

	t.Run("initial fetch populates cache", func(t *testing.T) {
		mock.ResetRequestCount()

		// Start should fetch tokens
		if err := validator.Start(); err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		defer validator.Stop()

		// Cache should have the token
		if validator.CacheSize() == 0 {
			t.Error("expected cache to be populated after Start")
		}

		// Should have made one request to fetch tokens
		if mock.GetRequestCount() != 1 {
			t.Errorf("expected 1 request for initial fetch, got %d", mock.GetRequestCount())
		}
	})

	t.Run("validate cached token locally", func(t *testing.T) {
		mock.ResetRequestCount()

		info, err := validator.ValidateToken(validToken, "org-456")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if info.UserID != "user-123" {
			t.Errorf("expected UserID user-123, got %s", info.UserID)
		}
	})

	t.Run("invalid token triggers refresh then fails", func(t *testing.T) {
		mock.ResetRequestCount()

		_, err := validator.ValidateToken("invalid-token", "org-456")
		if err != ErrInvalidToken {
			t.Errorf("expected ErrInvalidToken, got %v", err)
		}

		// Should have made a refresh attempt
		if mock.GetRequestCount() < 1 {
			t.Error("expected at least 1 request for refresh on cache miss")
		}
	})

	t.Run("wrong org ID fails", func(t *testing.T) {
		_, err := validator.ValidateToken(validToken, "wrong-org")
		if err != ErrUnauthorized {
			t.Errorf("expected ErrUnauthorized, got %v", err)
		}
	})
}

func TestCachingTokenValidatorExpiredToken(t *testing.T) {
	mock := StartMockAuthServer()
	defer mock.Close()

	// Add an expired token
	expiredToken := "expired-cached-token"
	mock.AddValidToken(expiredToken, &TokenInfo{
		UserID:    "user-123",
		OrgID:     "org-456",
		ExpiresAt: time.Now().Add(-time.Hour), // Expired
	})

	validator := NewCachingTokenValidator(mock.URL(), "org-456", "", time.Hour)
	validator.Start()
	defer validator.Stop()

	_, err := validator.ValidateToken(expiredToken, "org-456")
	if err != ErrTokenExpired {
		t.Errorf("expected ErrTokenExpired, got %v", err)
	}
}

func TestHashToken(t *testing.T) {
	// Test that hashToken produces consistent results
	hash1 := hashToken("test-token")
	hash2 := hashToken("test-token")
	if hash1 != hash2 {
		t.Error("hashToken should produce consistent results")
	}

	// Test that different tokens produce different hashes
	hash3 := hashToken("different-token")
	if hash1 == hash3 {
		t.Error("different tokens should produce different hashes")
	}

	// Verify hash length (SHA-256 = 64 hex chars)
	if len(hash1) != 64 {
		t.Errorf("expected hash length 64, got %d", len(hash1))
	}
}

func TestCachingTokenValidatorRateLimiting(t *testing.T) {
	mock := StartMockAuthServer()
	defer mock.Close()

	// Add one valid token
	mock.AddValidToken("valid-token", &TokenInfo{
		UserID:    "user-123",
		OrgID:     "org-456",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	// Create validator with short rate limit for testing
	validator := NewCachingTokenValidator(mock.URL(), "org-456", "", time.Hour)
	validator.refreshRateLimit = 100 * time.Millisecond // Short for testing
	validator.Start()
	defer validator.Stop()

	mock.ResetRequestCount()

	// First invalid token should trigger refresh
	_, err := validator.ValidateToken("invalid-token-1", "org-456")
	if err != ErrInvalidToken {
		t.Errorf("expected ErrInvalidToken, got %v", err)
	}
	firstCount := mock.GetRequestCount()
	if firstCount < 1 {
		t.Error("expected at least 1 request for first invalid token")
	}

	// Immediate second invalid token should NOT trigger refresh (rate limited)
	_, err = validator.ValidateToken("invalid-token-2", "org-456")
	if err != ErrInvalidToken {
		t.Errorf("expected ErrInvalidToken, got %v", err)
	}
	secondCount := mock.GetRequestCount()
	if secondCount != firstCount {
		t.Errorf("expected no additional requests due to rate limit, got %d (was %d)", secondCount, firstCount)
	}

	// Wait for rate limit to expire
	time.Sleep(150 * time.Millisecond)

	// Third invalid token should trigger refresh again
	_, err = validator.ValidateToken("invalid-token-3", "org-456")
	if err != ErrInvalidToken {
		t.Errorf("expected ErrInvalidToken, got %v", err)
	}
	thirdCount := mock.GetRequestCount()
	if thirdCount <= secondCount {
		t.Error("expected additional request after rate limit expired")
	}
}

// TestCachingTokenValidatorRotationGrace covers the #792 behavior: during a
// bounded rotation window the platform may carry an outgoing token's hash
// forward via TokenHashEntry.PreviousHash/PreviousHashExpiresAt, and the node
// should accept EITHER the current or the still-in-grace previous token.
func TestCachingTokenValidatorRotationGrace(t *testing.T) {
	mock := StartMockAuthServer()
	defer mock.Close()

	currentToken := "current-token-after-rotation"
	previousToken := "previous-token-before-rotation"

	mock.AddValidToken(currentToken, &TokenInfo{
		UserID:    "user-123",
		OrgID:     "org-456",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	mock.SetPreviousToken(currentToken, previousToken, time.Now().Add(time.Hour))

	validator := NewCachingTokenValidator(mock.URL(), "org-456", "", time.Hour)
	if err := validator.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer validator.Stop()

	t.Run("current token accepted", func(t *testing.T) {
		info, err := validator.ValidateToken(currentToken, "org-456")
		if err != nil {
			t.Fatalf("expected no error for current token, got %v", err)
		}
		if info.UserID != "user-123" {
			t.Errorf("expected UserID user-123, got %s", info.UserID)
		}
	})

	t.Run("previous-in-grace token accepted", func(t *testing.T) {
		info, err := validator.ValidateToken(previousToken, "org-456")
		if err != nil {
			t.Fatalf("expected no error for previous-in-grace token, got %v", err)
		}
		if info.UserID != "user-123" {
			t.Errorf("expected UserID user-123, got %s", info.UserID)
		}
	})

	t.Run("wrong token still rejected", func(t *testing.T) {
		_, err := validator.ValidateToken("some-other-token", "org-456")
		if err != ErrInvalidToken {
			t.Errorf("expected ErrInvalidToken, got %v", err)
		}
	})
}

// TestCachingTokenValidatorRotationGraceExpired verifies the previous hash is
// rejected, like any invalid token, once its grace window has elapsed.
func TestCachingTokenValidatorRotationGraceExpired(t *testing.T) {
	mock := StartMockAuthServer()
	defer mock.Close()

	currentToken := "current-token-2"
	previousToken := "previous-token-2-expired"

	mock.AddValidToken(currentToken, &TokenInfo{
		UserID:    "user-123",
		OrgID:     "org-456",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	// Grace window already in the past.
	mock.SetPreviousToken(currentToken, previousToken, time.Now().Add(-time.Minute))

	validator := NewCachingTokenValidator(mock.URL(), "org-456", "", time.Hour)
	// Speed up the on-demand refresh path so the cache-miss retry doesn't
	// need to wait out the default rate limit.
	validator.refreshRateLimit = 0
	if err := validator.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer validator.Stop()

	_, err := validator.ValidateToken(previousToken, "org-456")
	if err != ErrInvalidToken {
		t.Errorf("expected ErrInvalidToken for expired-grace previous token, got %v", err)
	}
}

// TestCachingTokenValidatorPreviousHashAbsentIsNoRegression pins the
// forward-compatibility contract: when the platform response carries no
// PreviousHash/PreviousHashExpiresAt (today's reality), behavior is
// byte-for-byte identical to before this change - only the current hash is
// ever accepted.
func TestCachingTokenValidatorPreviousHashAbsentIsNoRegression(t *testing.T) {
	mock := StartMockAuthServer()
	defer mock.Close()

	currentToken := "current-token-3-no-grace"
	someOtherToken := "unrelated-token-not-in-cache"

	mock.AddValidToken(currentToken, &TokenInfo{
		UserID:    "user-123",
		OrgID:     "org-456",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	// Deliberately do NOT call SetPreviousToken - mirrors today's API response.

	validator := NewCachingTokenValidator(mock.URL(), "org-456", "", time.Hour)
	if err := validator.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer validator.Stop()

	if _, err := validator.ValidateToken(currentToken, "org-456"); err != nil {
		t.Fatalf("expected current token to validate, got %v", err)
	}

	if _, err := validator.ValidateToken(someOtherToken, "org-456"); err != ErrInvalidToken {
		t.Errorf("expected ErrInvalidToken for a token with no grace window, got %v", err)
	}

	if len(validator.previousCache) != 0 {
		t.Errorf("expected previousCache to stay empty when platform sends no PreviousHash, got %d entries", len(validator.previousCache))
	}
}

// TestCachingTokenValidatorPreviousHashNoExpiryNotAccepted pins the "set hash
// but no expiry" edge case: PreviousHash present with a zero
// PreviousHashExpiresAt must NOT create an unbounded grace window - only the
// current hash is accepted, exactly like the absent-fields case.
func TestCachingTokenValidatorPreviousHashNoExpiryNotAccepted(t *testing.T) {
	mock := StartMockAuthServer()
	defer mock.Close()

	currentToken := "current-token-4"
	previousToken := "previous-token-4-no-expiry"

	mock.AddValidToken(currentToken, &TokenInfo{
		UserID:    "user-123",
		OrgID:     "org-456",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	// Zero-value expiry: the entry has a PreviousHash but no bound on it.
	mock.SetPreviousToken(currentToken, previousToken, time.Time{})

	validator := NewCachingTokenValidator(mock.URL(), "org-456", "", time.Hour)
	validator.refreshRateLimit = 0
	if err := validator.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer validator.Stop()

	_, err := validator.ValidateToken(previousToken, "org-456")
	if err != ErrInvalidToken {
		t.Errorf("expected ErrInvalidToken for previous hash with no expiry, got %v", err)
	}
}

func TestCachingTokenValidatorOrgIsolation(t *testing.T) {
	mock := StartMockAuthServer()
	defer mock.Close()

	// Add token for org-A
	mock.AddValidToken("org-a-token", &TokenInfo{
		UserID:    "user-123",
		OrgID:     "org-A",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	// Create validator for org-B
	validator := NewCachingTokenValidator(mock.URL(), "org-B", "", time.Hour)
	validator.Start()
	defer validator.Stop()

	// Attempt to use org-A's token with org-B validator
	// Token won't be in cache (different org), and even if it were, org check should fail
	_, err := validator.ValidateToken("org-a-token", "org-B")
	if err != ErrInvalidToken && err != ErrUnauthorized {
		t.Errorf("expected ErrInvalidToken or ErrUnauthorized for cross-org access, got %v", err)
	}
}

// TestTokenHashEntryMalformedPreviousHashExpiresAtDoesNotRejectEntry pins the
// #815 fix directly against TokenHashEntry.UnmarshalJSON: a malformed
// previous_hash_expires_at (non-RFC3339 string) must not error the entry -
// it must degrade to "no grace window" (PreviousHashExpiresAt left zero)
// while the current-hash fields still decode normally.
func TestTokenHashEntryMalformedPreviousHashExpiresAtDoesNotRejectEntry(t *testing.T) {
	raw := []byte(`{
		"hash": "abc123",
		"user_id": "user-1",
		"org_id": "org-1",
		"expires_at": "2026-01-01T00:00:00Z",
		"previous_hash": "def456",
		"previous_hash_expires_at": "not-a-timestamp"
	}`)

	var entry TokenHashEntry
	if err := entry.UnmarshalJSON(raw); err != nil {
		t.Fatalf("expected malformed previous_hash_expires_at to be tolerated, got error: %v", err)
	}

	if entry.Hash != "abc123" || entry.UserID != "user-1" || entry.OrgID != "org-1" {
		t.Errorf("expected current-hash fields to parse normally, got %+v", entry)
	}
	if !entry.ExpiresAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("expected expires_at to parse normally, got %v", entry.ExpiresAt)
	}
	if entry.PreviousHash != "def456" {
		t.Errorf("expected previous_hash to still be captured, got %q", entry.PreviousHash)
	}
	if !entry.PreviousHashExpiresAt.IsZero() {
		t.Errorf("expected malformed previous_hash_expires_at to degrade to zero (no grace window), got %v", entry.PreviousHashExpiresAt)
	}
}

// TestCachingTokenValidatorMalformedPreviousHashExpiresAtColdStart is the
// full end-to-end regression for #815: on a COLD START (no prior cache), a
// token-list response containing one entry with a malformed
// previous_hash_expires_at must still populate the cache with EVERY valid
// current-hash entry - including the malformed one's own current hash - not
// leave the cache empty and reject every connection.
func TestCachingTokenValidatorMalformedPreviousHashExpiresAtColdStart(t *testing.T) {
	goodToken := "good-token-only"
	badGraceToken := "token-with-bad-grace"
	previousOfBadGrace := "previous-of-bad-grace-token"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"tokens": [
				{
					"hash": %q,
					"user_id": "user-good",
					"org_id": "org-1",
					"expires_at": "2099-01-01T00:00:00Z"
				},
				{
					"hash": %q,
					"user_id": "user-bad-grace",
					"org_id": "org-1",
					"expires_at": "2099-01-01T00:00:00Z",
					"previous_hash": %q,
					"previous_hash_expires_at": "not-a-timestamp"
				}
			]
		}`, hashToken(goodToken), hashToken(badGraceToken), hashToken(previousOfBadGrace))
	}))
	defer server.Close()

	validator := NewCachingTokenValidator(server.URL, "org-1", "", time.Hour)
	if err := validator.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer validator.Stop()

	if validator.CacheSize() != 2 {
		t.Fatalf("expected cold-start decode to keep both current-hash entries despite the malformed grace field, got cache size %d", validator.CacheSize())
	}

	if _, err := validator.ValidateToken(goodToken, "org-1"); err != nil {
		t.Errorf("expected the entry unaffected by the malformed field to validate, got %v", err)
	}
	if _, err := validator.ValidateToken(badGraceToken, "org-1"); err != nil {
		t.Errorf("expected the current hash of the entry WITH the malformed grace field to still validate, got %v", err)
	}

	// The grace window itself must not be honored - it was malformed, so it
	// degrades to "no grace window" rather than being silently accepted.
	if _, err := validator.ValidateToken(previousOfBadGrace, "org-1"); err != ErrInvalidToken {
		t.Errorf("expected the previous-hash to be rejected (malformed grace never grants a window), got %v", err)
	}

	if len(validator.previousCache) != 0 {
		t.Errorf("expected previousCache to stay empty when the only grace field present was malformed, got %d entries", len(validator.previousCache))
	}
}
