package egressrelay

import "testing"

func TestIsDestinationAllowed(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		allowLAN bool
		want     bool
	}{
		{"public ip denied-LAN allowed", "8.8.8.8", false, true},
		{"public hostname denied-LAN allowed", "example.com", false, true},
		{"loopback ip denied by default", "127.0.0.1", false, false},
		{"loopback ipv6 denied by default", "::1", false, false},
		{"localhost name denied by default", "localhost", false, false},
		{"link-local denied by default", "169.254.1.1", false, false},
		{"rfc1918 10/8 denied by default", "10.0.0.5", false, false},
		{"rfc1918 172.16/12 denied by default", "172.16.5.5", false, false},
		{"rfc1918 192.168/16 denied by default", "192.168.1.1", false, false},
		{"cgnat/mesh denied by default", "100.64.0.147", false, false},
		{"cgnat/mesh boundary denied by default", "100.127.255.254", false, false},
		{"just outside cgnat allowed", "100.128.0.1", false, true},

		{"loopback allowed with allow_lan", "127.0.0.1", true, true},
		{"localhost name allowed with allow_lan", "localhost", true, true},
		{"rfc1918 allowed with allow_lan", "192.168.1.1", true, true},
		{"cgnat/mesh allowed with allow_lan", "100.64.0.147", true, true},
		{"link-local allowed with allow_lan", "169.254.1.1", true, true},
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

func TestIsDestinationAllowedUnresolvedHostnameIsAllowed(t *testing.T) {
	// A non-IP hostname is not resolved by this function -- see its doc
	// comment. This pins that deliberate scope so a future change doesn't
	// silently start refusing every hostname (which would break ordinary
	// public-internet egress, the relay's primary purpose).
	ok, reason := IsDestinationAllowed("internal-service.corp.example", false)
	if !ok {
		t.Fatalf("expected unresolved hostname to be allowed, got denied: %s", reason)
	}
}
