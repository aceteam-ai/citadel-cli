// internal/network/keyhealth_test.go
package network

import (
	"testing"
	"time"
)

func TestKeyRenewalDecision(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	threshold := 24 * time.Hour

	ptr := func(t time.Time) *time.Time { return &t }

	tests := []struct {
		name      string
		keyExpiry *time.Time
		want      KeyStatus
	}{
		{
			name:      "nil expiry is the durable-baseline no-op",
			keyExpiry: nil,
			want:      KeyNoExpiry,
		},
		{
			name:      "zero expiry is treated as no expiry",
			keyExpiry: ptr(time.Time{}),
			want:      KeyNoExpiry,
		},
		{
			name:      "expiry far in the future is healthy",
			keyExpiry: ptr(now.Add(10 * 24 * time.Hour)),
			want:      KeyHealthy,
		},
		{
			name:      "expiry just past the threshold is healthy",
			keyExpiry: ptr(now.Add(threshold + time.Minute)),
			want:      KeyHealthy,
		},
		{
			name:      "expiry exactly at the threshold renews",
			keyExpiry: ptr(now.Add(threshold)),
			want:      KeyRenewSoon,
		},
		{
			name:      "expiry within the threshold renews",
			keyExpiry: ptr(now.Add(2 * time.Hour)),
			want:      KeyRenewSoon,
		},
		{
			name:      "expiry exactly now is expired",
			keyExpiry: ptr(now),
			want:      KeyExpired,
		},
		{
			name:      "expiry in the past is expired",
			keyExpiry: ptr(now.Add(-time.Hour)),
			want:      KeyExpired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := keyRenewalDecision(tt.keyExpiry, now, threshold)
			if got != tt.want {
				t.Errorf("keyRenewalDecision() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKeyStatusString(t *testing.T) {
	cases := map[KeyStatus]string{
		KeyNoExpiry:   "no-expiry",
		KeyHealthy:    "healthy",
		KeyRenewSoon:  "renew-soon",
		KeyExpired:    "expired",
		KeyStatus(99): "unknown",
	}
	for status, want := range cases {
		if got := status.String(); got != want {
			t.Errorf("KeyStatus(%d).String() = %q, want %q", status, got, want)
		}
	}
}

func TestInspectGlobalNodeKey_NotConnected(t *testing.T) {
	// With no global server set, inspection returns nil health, no error —
	// there is no key to inspect.
	ClearGlobal()
	health, err := InspectGlobalNodeKey(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if health != nil {
		t.Errorf("expected nil health when not connected, got %+v", health)
	}
}

func TestRenewGlobalNodeKey_Guards(t *testing.T) {
	ClearGlobal()

	// Empty authkey is rejected before touching any server.
	if err := RenewGlobalNodeKey(t.Context(), ""); err == nil {
		t.Error("expected error for empty authkey")
	}

	// Not connected is rejected.
	if err := RenewGlobalNodeKey(t.Context(), "tskey-fake"); err == nil {
		t.Error("expected error when not connected")
	}
}
