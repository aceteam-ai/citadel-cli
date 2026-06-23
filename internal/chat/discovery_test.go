package chat

import (
	"strings"
	"testing"
)

func TestSanitizeEndpoint(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"https to wss", "https://aceteam.ai", "wss://aceteam.ai"},
		{"http to ws", "http://localhost:8000", "ws://localhost:8000"},
		{"strips path", "https://aceteam.ai/api/fabric/redis/ws", "wss://aceteam.ai"},
		{"keeps port", "https://staging.aceteam.ai:8443/foo", "wss://staging.aceteam.ai:8443"},
		{"already wss", "wss://aceteam.ai/x", "wss://aceteam.ai"},
		{"empty", "", ""},
		{"no host", "not a url", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeEndpoint(tt.in)
			if got != tt.want {
				t.Fatalf("SanitizeEndpoint(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSanitizeEndpointNeverLeaksTransport guards the issue #293 requirement
// that the user-facing endpoint label must not reveal the Redis transport.
func TestSanitizeEndpointNeverLeaksTransport(t *testing.T) {
	inputs := []string{
		"https://aceteam.ai/api/fabric/redis/ws",
		"http://localhost:8000/api/fabric/redis/ws",
		"https://aceteam.ai/redis",
	}
	for _, in := range inputs {
		got := SanitizeEndpoint(in)
		if strings.Contains(strings.ToLower(got), "redis") {
			t.Fatalf("SanitizeEndpoint(%q) = %q leaks transport path", in, got)
		}
		if strings.Contains(got, "/") && !strings.Contains(got, "://") {
			t.Fatalf("SanitizeEndpoint(%q) = %q unexpectedly contains a path", in, got)
		}
	}
}

func TestEndpointURLPassthrough(t *testing.T) {
	c := NewClient(ClientConfig{APIBaseURL: "https://aceteam.ai", Token: "t", OrgID: "o"})
	if got := c.EndpointURL(); got != "wss://aceteam.ai" {
		t.Fatalf("EndpointURL() = %q, want wss://aceteam.ai", got)
	}
}

// TestProbeResultSpeaksChatStub documents the design invariant: until a
// per-node chat listener exists, discovery never claims a peer speaks chat.
func TestProbeResultSpeaksChatStub(t *testing.T) {
	r := ProbeResult{NodeName: "n", Reachable: true}
	if r.SpeaksChat {
		t.Fatal("SpeaksChat must default to false; no per-node listener exists yet")
	}
}
