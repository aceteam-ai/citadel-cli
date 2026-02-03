package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/e2e/harness"
)

// TestDeviceAuthFlow tests the OAuth 2.0 device authorization flow
func TestDeviceAuthFlow(t *testing.T) {
	aceteamURL := os.Getenv("ACETEAM_URL")
	if aceteamURL == "" {
		aceteamURL = "http://localhost:3000"
	}

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Initialize harnesses
	aceteam := harness.NewAceTeamHarness(aceteamURL)
	redis, err := harness.NewRedisHarness(redisURL)
	if err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	defer redis.Close()

	// Wait for AceTeam to be ready
	t.Log("Waiting for AceTeam API to be ready...")
	if err := aceteam.WaitForReady(ctx, 60*time.Second); err != nil {
		t.Skipf("AceTeam API not ready: %v", err)
	}

	t.Run("StartFlow", func(t *testing.T) {
		// Start device authorization flow
		codeResp, err := aceteam.StartDeviceAuth(ctx)
		if err != nil {
			t.Fatalf("Failed to start device auth: %v", err)
		}

		// Validate response
		if codeResp.DeviceCode == "" {
			t.Error("Expected device_code to be non-empty")
		}
		if codeResp.UserCode == "" {
			t.Error("Expected user_code to be non-empty")
		}
		if codeResp.ExpiresIn <= 0 {
			t.Errorf("Expected expires_in > 0, got %d", codeResp.ExpiresIn)
		}
		if codeResp.Interval <= 0 {
			t.Errorf("Expected interval > 0, got %d", codeResp.Interval)
		}

		t.Logf("Device code: %s", codeResp.DeviceCode)
		t.Logf("User code: %s", codeResp.UserCode)
	})

	t.Run("PollPending", func(t *testing.T) {
		// Start a new flow
		codeResp, err := aceteam.StartDeviceAuth(ctx)
		if err != nil {
			t.Fatalf("Failed to start device auth: %v", err)
		}

		// Poll before approval - should get authorization_pending
		tokenResp, tokenErr, err := aceteam.PollForToken(ctx, codeResp.DeviceCode)
		if err != nil {
			t.Fatalf("Poll request failed: %v", err)
		}

		if tokenResp != nil {
			t.Error("Expected no token response before approval")
		}
		if tokenErr == nil {
			t.Error("Expected error response before approval")
		} else if tokenErr.ErrorCode != "authorization_pending" {
			t.Errorf("Expected authorization_pending error, got %s", tokenErr.ErrorCode)
		}
	})

	t.Run("ApprovalFlow", func(t *testing.T) {
		// This test simulates the approval via Redis directly
		// (In real usage, the user approves via the web UI)

		// Start a new flow
		codeResp, err := aceteam.StartDeviceAuth(ctx)
		if err != nil {
			t.Fatalf("Failed to start device auth: %v", err)
		}

		// Verify device state in Redis
		state, err := redis.GetDeviceAuthState(ctx, codeResp.DeviceCode)
		if err != nil {
			t.Fatalf("Failed to get device state: %v", err)
		}
		if state == nil {
			t.Skip("Device auth state not stored in Redis (may use different storage)")
		}

		t.Logf("Device state: %+v", state)
	})
}

// TestDeviceAuthExpiry tests that expired device codes are rejected
func TestDeviceAuthExpiry(t *testing.T) {
	aceteamURL := os.Getenv("ACETEAM_URL")
	if aceteamURL == "" {
		aceteamURL = "http://localhost:3000"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	aceteam := harness.NewAceTeamHarness(aceteamURL)

	// Poll with a fake/expired device code
	tokenResp, tokenErr, err := aceteam.PollForToken(ctx, "fake-expired-code-12345")
	if err != nil {
		t.Fatalf("Poll request failed: %v", err)
	}

	if tokenResp != nil {
		t.Error("Expected no token for invalid device code")
	}
	if tokenErr == nil {
		t.Error("Expected error for invalid device code")
	} else {
		t.Logf("Got expected error: %s - %s", tokenErr.ErrorCode, tokenErr.ErrorDescription)
	}
}
