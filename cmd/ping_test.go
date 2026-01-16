// cmd/ping_test.go
package cmd

import (
	"testing"
)

func TestIsIPAddress(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		// Valid IPv4 addresses
		{"valid IPv4", "192.168.1.1", true},
		{"valid IPv4 zeros", "0.0.0.0", true},
		{"valid IPv4 broadcast", "255.255.255.255", true},
		{"valid IPv4 localhost", "127.0.0.1", true},
		{"valid tailscale IP", "100.64.0.25", true},

		// Valid IPv6 addresses
		{"valid IPv6 localhost", "::1", true},
		{"valid IPv6 full", "2001:0db8:85a3:0000:0000:8a2e:0370:7334", true},
		{"valid IPv6 compressed", "2001:db8::1", true},

		// Invalid inputs (should be treated as hostnames)
		{"hostname simple", "aceteamvm", false},
		{"hostname with dash", "ubuntu-gpu", false},
		{"hostname FQDN", "server.example.com", false},
		{"hostname with numbers", "server123", false},
		{"empty string", "", false},
		{"just dots", "...", false},
		{"invalid IP too many octets", "1.2.3.4.5", false},
		{"invalid IP octet too large", "999.999.999.999", false},
		{"invalid IP negative", "-1.0.0.0", false},
		{"invalid IP with letters", "192.168.1.abc", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isIPAddress(tt.input)
			if got != tt.want {
				t.Errorf("isIPAddress(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtractChannelName(t *testing.T) {
	tests := []struct {
		name      string
		queueName string
		want      string
	}{
		{"standard queue name", "jobs:v1:gpu-general", "gpu-general"},
		{"different channel", "jobs:v1:cpu-workers", "cpu-workers"},
		{"two parts only", "jobs:queue", "jobs:queue"}, // Only extracts if >= 3 parts
		{"single part", "queue", "queue"},
		{"empty string", "", ""},
		{"many colons", "a:b:c:d:e:f", "f"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractChannelName(tt.queueName)
			if got != tt.want {
				t.Errorf("extractChannelName(%q) = %q, want %q", tt.queueName, got, tt.want)
			}
		})
	}
}

func TestGetDLQName(t *testing.T) {
	tests := []struct {
		name      string
		queueName string
		want      string
	}{
		{"standard queue", "jobs:v1:gpu-general", "dlq:v1:gpu-general"},
		{"different channel", "jobs:v1:cpu-workers", "dlq:v1:cpu-workers"},
		{"single part", "myqueue", "dlq:v1:myqueue"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getDLQName(tt.queueName)
			if got != tt.want {
				t.Errorf("getDLQName(%q) = %q, want %q", tt.queueName, got, tt.want)
			}
		})
	}
}

func TestMaxPingCount(t *testing.T) {
	// Verify the constant is set to a reasonable value
	if maxPingCount < 1 {
		t.Errorf("maxPingCount should be at least 1, got %d", maxPingCount)
	}
	if maxPingCount > 1000 {
		t.Errorf("maxPingCount should not exceed 1000, got %d", maxPingCount)
	}
}
