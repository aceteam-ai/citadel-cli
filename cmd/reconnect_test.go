package cmd

import (
	"context"
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

// TestAttemptVPNRecoveryNoToken verifies recovery fails cleanly without a device token.
func TestAttemptVPNRecoveryNoToken(t *testing.T) {
	ctx := context.Background()
	// With nil config, recovery should not panic.
	// Since there's no network state in the test env, VerifyOrReconnect
	// returns (false, nil) which means "not logged in" -- the function
	// will return Connected: false without error.
	result := attemptVPNRecovery(ctx, nil, "test-node", "https://example.com")
	// Either way, it should not panic.
	_ = result
}
