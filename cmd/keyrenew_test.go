// cmd/keyrenew_test.go
package cmd

import (
	"testing"
	"time"
)

func TestKeyRenewIntervalFromEnv(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		wantEnabled bool
		wantDur     time.Duration
	}{
		{
			name:        "unset uses default and is enabled",
			env:         "",
			wantEnabled: true,
			wantDur:     keyRenewCheckInterval,
		},
		{
			name:        "positive value overrides interval",
			env:         "300",
			wantEnabled: true,
			wantDur:     300 * time.Second,
		},
		{
			name:        "zero disables the loop",
			env:         "0",
			wantEnabled: false,
		},
		{
			name:        "negative disables the loop",
			env:         "-1",
			wantEnabled: false,
		},
		{
			name:        "non-numeric falls back to default enabled",
			env:         "abc",
			wantEnabled: true,
			wantDur:     keyRenewCheckInterval,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env == "" {
				t.Setenv(keyRenewEnvInterval, "")
			} else {
				t.Setenv(keyRenewEnvInterval, tt.env)
			}
			dur, enabled := keyRenewIntervalFromEnv()
			if enabled != tt.wantEnabled {
				t.Fatalf("enabled = %v, want %v", enabled, tt.wantEnabled)
			}
			if tt.wantEnabled && dur != tt.wantDur {
				t.Errorf("duration = %v, want %v", dur, tt.wantDur)
			}
		})
	}
}

// TestStartNodeKeyRenewer_NoDeviceToken verifies the renewer is a safe no-op
// when there is no device token (direct-Redis mode). It must not panic or start
// a goroutine that would fail.
func TestStartNodeKeyRenewer_NoDeviceToken(t *testing.T) {
	// nil config -> no-op
	startNodeKeyRenewer(t.Context(), nil)

	// config without a token -> no-op
	startNodeKeyRenewer(t.Context(), &DeviceConfig{})
}

// TestNoKeyRenewFlagRegistered verifies the --no-key-renew opt-out is wired up.
func TestNoKeyRenewFlagRegistered(t *testing.T) {
	flag := workCmd.Flags().Lookup("no-key-renew")
	if flag == nil {
		t.Fatal("--no-key-renew flag not registered on work command")
	}
	if flag.DefValue != "false" {
		t.Errorf("--no-key-renew default = %q, want %q", flag.DefValue, "false")
	}
}
