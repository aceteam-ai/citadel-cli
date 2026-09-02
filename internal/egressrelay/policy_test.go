package egressrelay

import "testing"

func TestIsDestinationAllowed(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		allowLAN bool
		want     bool
	}{
		{"public ip allowed without allow_lan", "8.8.8.8", false, true},
		// A bare hostname is refused BY THIS FUNCTION regardless of whether it
		// would resolve to something public -- this function makes no network
		// calls, so it cannot tell. The real "is this hostname's resolved
		// address public" decision is made by PolicyDialer (relay_test.go),
		// which resolves first and calls this function with the literal IP.
		{"unresolved hostname denied by default (no resolution here)", "example.com", false, false},
		{"loopback ip denied by default", "127.0.0.1", false, false},
		{"loopback ipv6 denied by default", "::1", false, false},
		{"localhost name denied by default", "localhost", false, false},
		{"unspecified ipv4 denied by default", "0.0.0.0", false, false},
		{"unspecified ipv6 denied by default", "::", false, false},
		{"link-local denied by default", "169.254.1.1", false, false},
		{"rfc1918 10/8 denied by default", "10.0.0.5", false, false},
		{"rfc1918 172.16/12 denied by default", "172.16.5.5", false, false},
		{"rfc1918 192.168/16 denied by default", "192.168.1.1", false, false},
		{"cgnat/mesh denied by default", "100.64.0.147", false, false},
		{"cgnat/mesh boundary denied by default", "100.127.255.254", false, false},
		{"just outside cgnat allowed", "100.128.0.1", false, true},

		{"loopback allowed with allow_lan", "127.0.0.1", true, true},
		{"localhost name allowed with allow_lan", "localhost", true, true},
		{"unspecified ipv4 allowed with allow_lan", "0.0.0.0", true, true},
		{"unspecified ipv6 allowed with allow_lan", "::", true, true},
		{"rfc1918 allowed with allow_lan", "192.168.1.1", true, true},
		{"cgnat/mesh allowed with allow_lan", "100.64.0.147", true, true},
		{"link-local allowed with allow_lan", "169.254.1.1", true, true},
		{"unresolved hostname allowed with allow_lan", "example.com", true, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := IsDestinationAllowed(tc.host, tc.allowLAN)
			if got != tc.want {
				t.Fatalf("IsDestinationAllowed(%q, %v) = %v (%q), want %v", tc.host, tc.allowLAN, got, reason, tc.want)
			}
			if !got && reason == "" {
				t.Fatalf("IsDestinationAllowed(%q, %v) denied with no reason", tc.host, tc.allowLAN)
			}
		})
	}
}

// TestIsDestinationAllowedUnresolvedHostnameDeniedByDefault pins the
// fail-closed fix for citadel #787's hostname->private-IP bypass: this
// function has no resolver, so it must refuse an unresolved hostname rather
// than assume it's safe. Before this fix it returned `true` here ("DNS
// resolution happens inside the underlying dialer"), which is exactly what
// let a CONNECT to a hostname with a private A-record (or a DNS-rebinding
// attacker) sail through untouched -- the real fix moves resolution INTO the
// policy check via PolicyDialer (see relay_test.go's
// TestPolicyDialer*Hostname* tests for the end-to-end version of this).
func TestIsDestinationAllowedUnresolvedHostnameDeniedByDefault(t *testing.T) {
	ok, reason := IsDestinationAllowed("internal-service.corp.example", false)
	if ok {
		t.Fatal("expected unresolved hostname to be denied by default (no resolution happens in this function)")
	}
	if reason == "" {
		t.Fatal("expected a reason for the denial")
	}
}
