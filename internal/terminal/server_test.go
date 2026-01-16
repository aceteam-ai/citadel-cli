// internal/terminal/server_test.go
package terminal

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestNewServer(t *testing.T) {
	config := &Config{
		Port:           7860,
		MaxConnections: 10,
		IdleTimeout:    30 * time.Minute,
		OrgID:          "test-org",
		Shell:          "/bin/sh",
		RateLimitRPS:   1.0,
		RateLimitBurst: 5,
	}

	auth := NewMockTokenValidator()
	server := NewServer(config, auth)

	if server == nil {
		t.Fatal("expected server, got nil")
	}

	if server.Port() != 7860 {
		t.Errorf("expected port 7860, got %d", server.Port())
	}

	if server.IsRunning() {
		t.Error("server should not be running before Start()")
	}
}

func TestServerStartStop(t *testing.T) {
	config := &Config{
		Port:           0, // Use any available port
		MaxConnections: 10,
		IdleTimeout:    30 * time.Minute,
		OrgID:          "test-org",
		Shell:          "/bin/sh",
		RateLimitRPS:   1.0,
		RateLimitBurst: 5,
	}

	// Find a free port
	config.Port = 17860 // Use a high port that's likely free

	auth := NewMockTokenValidator()
	server := NewServer(config, auth)

	// Start server
	err := server.Start()
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if !server.IsRunning() {
		t.Error("server should be running after Start()")
	}

	// Try to start again (should fail)
	err = server.Start()
	if err != ErrServerAlreadyRunning {
		t.Errorf("expected ErrServerAlreadyRunning, got %v", err)
	}

	// Stop server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = server.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if server.IsRunning() {
		t.Error("server should not be running after Stop()")
	}

	// Try to stop again (should fail)
	err = server.Stop(ctx)
	if err != ErrServerNotRunning {
		t.Errorf("expected ErrServerNotRunning, got %v", err)
	}
}

func TestServerHealth(t *testing.T) {
	config := &Config{
		Port:           17861, // Use a different port
		MaxConnections: 10,
		IdleTimeout:    30 * time.Minute,
		OrgID:          "test-org",
		Shell:          "/bin/sh",
		RateLimitRPS:   1.0,
		RateLimitBurst: 5,
	}

	auth := NewMockTokenValidator()
	server := NewServer(config, auth)

	err := server.Start()
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer server.Stop(context.Background())

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Test health endpoint
	resp, err := http.Get("http://localhost:17861/health")
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestServerSessionCount(t *testing.T) {
	config := &Config{
		Port:           17862,
		MaxConnections: 10,
		IdleTimeout:    30 * time.Minute,
		OrgID:          "test-org",
		Shell:          "/bin/sh",
		RateLimitRPS:   1.0,
		RateLimitBurst: 5,
	}

	auth := NewMockTokenValidator()
	server := NewServer(config, auth)

	// Session count should be 0 before starting
	if server.SessionCount() != 0 {
		t.Errorf("expected session count 0, got %d", server.SessionCount())
	}
}

func TestGetClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		xri        string
		expected   string
	}{
		{
			name:       "from remote addr",
			remoteAddr: "192.168.1.1:12345",
			expected:   "192.168.1.1",
		},
		{
			name:       "from X-Forwarded-For",
			remoteAddr: "10.0.0.1:12345",
			xff:        "203.0.113.50",
			expected:   "203.0.113.50",
		},
		{
			name:       "from X-Forwarded-For with multiple IPs",
			remoteAddr: "10.0.0.1:12345",
			xff:        "203.0.113.50, 70.41.3.18, 150.172.238.178",
			expected:   "203.0.113.50",
		},
		{
			name:       "from X-Real-IP",
			remoteAddr: "10.0.0.1:12345",
			xri:        "203.0.113.75",
			expected:   "203.0.113.75",
		},
		{
			name:       "X-Forwarded-For takes precedence",
			remoteAddr: "10.0.0.1:12345",
			xff:        "203.0.113.50",
			xri:        "203.0.113.75",
			expected:   "203.0.113.50",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &http.Request{
				RemoteAddr: tt.remoteAddr,
				Header:     make(http.Header),
			}
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xri != "" {
				req.Header.Set("X-Real-IP", tt.xri)
			}

			ip := getClientIP(req)
			if ip != tt.expected {
				t.Errorf("getClientIP() = %s, want %s", ip, tt.expected)
			}
		})
	}
}
