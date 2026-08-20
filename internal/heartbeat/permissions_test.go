package heartbeat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPermissionStateHasPasscodeJSONKey pins the exact wire key for
// PermissionState.HasPasscode (citadel #758). There is no live backend
// consumer yet (aceteam PR #7532 ships the dashboard side as a documented
// follow-up), so nothing else in this repo enforces the spelling — this test
// is the pin a future consumer-side PR must match. false is written (no
// omitempty), matching every sibling field in this struct: "not set" is a
// meaningful state, not an absent one.
func TestPermissionStateHasPasscodeJSONKey(t *testing.T) {
	cases := []struct {
		name string
		val  bool
		want string
	}{
		{"true", true, `"has_passcode":true`},
		{"false", false, `"has_passcode":false`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(PermissionState{HasPasscode: tc.val})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(data), tc.want) {
				t.Errorf("PermissionState JSON = %s, want substring %s", data, tc.want)
			}
		})
	}
}

// TestRedisPublisherPermissionsProviderTakesPrecedence proves
// SetPermissionsProvider (re-read every heartbeat) wins over a static
// SetPermissions snapshot, and that currentPermissions calls the provider
// fresh each time rather than caching its first result -- the mechanism that
// lets HasPasscode reflect a passcode set/cleared by a separate process
// (`citadel passcode set`/`clear`) while this worker keeps running.
func TestRedisPublisherPermissionsProviderTakesPrecedence(t *testing.T) {
	p, err := NewRedisPublisher(RedisPublisherConfig{
		RedisURL: "redis://localhost:6379",
		NodeID:   "test-node",
	}, nil)
	if err != nil {
		t.Fatalf("NewRedisPublisher: %v", err)
	}

	if got := p.currentPermissions(); got != nil {
		t.Errorf("currentPermissions with nothing registered must be nil, got %+v", got)
	}

	p.SetPermissions(&PermissionState{HasPasscode: false})
	if got := p.currentPermissions(); got == nil || got.HasPasscode {
		t.Errorf("expected static snapshot HasPasscode=false, got %+v", got)
	}

	live := false
	p.SetPermissionsProvider(func() *PermissionState {
		return &PermissionState{HasPasscode: live}
	})
	if got := p.currentPermissions(); got == nil || got.HasPasscode {
		t.Errorf("expected provider HasPasscode=false, got %+v", got)
	}

	// Flip the underlying value the provider reads (simulating `citadel
	// passcode set` running out-of-process and rewriting permissions.yaml)
	// without calling SetPermissionsProvider again.
	live = true
	if got := p.currentPermissions(); got == nil || !got.HasPasscode {
		t.Errorf("expected provider to reflect the live change, HasPasscode=%+v", got)
	}
}

// TestAPIPublisherPermissionsProviderTakesPrecedence is the APIPublisher half
// of the same contract.
func TestAPIPublisherPermissionsProviderTakesPrecedence(t *testing.T) {
	p := &APIPublisher{}

	if got := p.currentPermissions(); got != nil {
		t.Errorf("currentPermissions with nothing registered must be nil, got %+v", got)
	}

	p.SetPermissions(&PermissionState{HasPasscode: false})
	if got := p.currentPermissions(); got == nil || got.HasPasscode {
		t.Errorf("expected static snapshot HasPasscode=false, got %+v", got)
	}

	live := false
	p.SetPermissionsProvider(func() *PermissionState {
		return &PermissionState{HasPasscode: live}
	})
	live = true
	if got := p.currentPermissions(); got == nil || !got.HasPasscode {
		t.Errorf("expected provider to reflect the live change, HasPasscode=%+v", got)
	}
}
