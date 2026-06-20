package cmd

import (
	"context"
	"strings"
	"testing"
)

// TestReconnectCommandRegistered verifies the reconnect command is wired into rootCmd.
func TestReconnectCommandRegistered(t *testing.T) {
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "reconnect" {
			found = true
			break
		}
	}
	if !found {
		t.Error("'reconnect' command not registered with rootCmd")
	}
}

// TestReconnectCommandFlags verifies the --force flag is registered.
func TestReconnectCommandFlags(t *testing.T) {
	flag := reconnectCmd.Flags().Lookup("force")
	if flag == nil {
		t.Error("--force flag not registered on reconnect command")
	}
	if flag.DefValue != "false" {
		t.Errorf("--force default = %q, want %q", flag.DefValue, "false")
	}
}

// TestVPNRecoveryResultTypes verifies the VPNRecoveryResult struct fields.
func TestVPNRecoveryResultTypes(t *testing.T) {
	// Success with IP preserved
	r1 := VPNRecoveryResult{Connected: true, IPPreserved: true}
	if !r1.Connected || !r1.IPPreserved || r1.Err != nil {
		t.Error("IP-preserved success result has wrong fields")
	}

	// Success with fresh state
	r2 := VPNRecoveryResult{Connected: true, IPPreserved: false}
	if !r2.Connected || r2.IPPreserved || r2.Err != nil {
		t.Error("fresh-state success result has wrong fields")
	}

	// Failure
	r3 := VPNRecoveryResult{Err: errTest}
	if r3.Connected || r3.IPPreserved || r3.Err == nil {
		t.Error("failure result has wrong fields")
	}
}

var errTest = &testError{"test error"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// TestRecoverStaleVPNNoToken verifies recovery fails with a clear error
// when no device API token is available. Uses recoverStaleVPN directly
// to avoid live VerifyOrReconnect I/O.
func TestRecoverStaleVPNNoToken(t *testing.T) {
	ctx := context.Background()

	// nil config: should return error about missing token
	r1 := recoverStaleVPN(ctx, nil, "test-node", "https://example.com")
	if r1.Connected {
		t.Error("expected Connected=false with nil config")
	}
	if r1.Err == nil || r1.Err.Error() != "no device API token available for auto-recovery" {
		t.Errorf("expected token error, got: %v", r1.Err)
	}

	// empty token: same result
	r2 := recoverStaleVPN(ctx, &DeviceConfig{DeviceAPIToken: ""}, "test-node", "https://example.com")
	if r2.Connected {
		t.Error("expected Connected=false with empty token")
	}
	if r2.Err == nil || r2.Err.Error() != "no device API token available for auto-recovery" {
		t.Errorf("expected token error, got: %v", r2.Err)
	}
}

// TestRecoverStaleVPNFetchAuthkeyFails verifies that recoverStaleVPN returns
// a clear error when FetchFreshAuthkey fails (e.g. network down, bad token).
func TestRecoverStaleVPNFetchAuthkeyFails(t *testing.T) {
	ctx := context.Background()

	// Use a config with a token but pointing to a non-existent server.
	// FetchFreshAuthkey will fail with a connection error.
	cfg := &DeviceConfig{DeviceAPIToken: "act_test_token_12345"}
	r := recoverStaleVPN(ctx, cfg, "test-node", "http://127.0.0.1:1")
	if r.Connected {
		t.Error("expected Connected=false when authkey fetch fails")
	}
	if r.Err == nil {
		t.Fatal("expected error when authkey fetch fails")
	}
	if !strings.Contains(r.Err.Error(), "could not fetch fresh authkey") {
		t.Errorf("expected authkey fetch error, got: %v", r.Err)
	}
}
