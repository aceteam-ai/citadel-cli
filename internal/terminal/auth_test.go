// internal/terminal/auth_test.go
package terminal

import (
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
