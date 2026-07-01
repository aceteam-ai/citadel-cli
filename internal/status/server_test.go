package status

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/terminal"
)

// mockTokenValidator is a test double for terminal.TokenValidator.
type mockTokenValidator struct {
	validToken string
}

func (m *mockTokenValidator) ValidateToken(token string, orgID string) (*terminal.TokenInfo, error) {
	if token == m.validToken {
		return &terminal.TokenInfo{UserID: "test-user", OrgID: orgID}, nil
	}
	return nil, fmt.Errorf("invalid token")
}

func TestNewServer(t *testing.T) {
	collector := NewCollector(CollectorConfig{
		NodeName: "test-node",
	})

	tests := []struct {
		name     string
		config   ServerConfig
		wantPort int
	}{
		{
			name:     "with default port",
			config:   ServerConfig{},
			wantPort: 8080,
		},
		{
			name: "with custom port",
			config: ServerConfig{
				Port: 9090,
			},
			wantPort: 9090,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServer(tt.config, collector)

			if server == nil {
				t.Fatal("NewServer returned nil")
			}
			if server.Port() != tt.wantPort {
				t.Errorf("Port() = %v, want %v", server.Port(), tt.wantPort)
			}
		})
	}
}

func TestServerHealthEndpoint(t *testing.T) {
	collector := NewCollector(CollectorConfig{
		NodeName: "test-node",
	})
	server := NewServer(ServerConfig{
		Port:    8080,
		Version: "1.0.0",
	}, collector)

	// Create test request
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	// Call handler directly
	server.handleHealth(w, req)

	// Check response
	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %v, want %v", resp.StatusCode, http.StatusOK)
	}

	var healthResp HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&healthResp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if healthResp.Status != HealthStatusOK {
		t.Errorf("Status = %v, want %v", healthResp.Status, HealthStatusOK)
	}
	if healthResp.Version != "1.0.0" {
		t.Errorf("Version = %v, want 1.0.0", healthResp.Version)
	}
}

func TestServerHealthEndpointMethodNotAllowed(t *testing.T) {
	collector := NewCollector(CollectorConfig{NodeName: "test"})
	server := NewServer(ServerConfig{}, collector)

	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("StatusCode = %v, want %v", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestServerStatusEndpoint(t *testing.T) {
	collector := NewCollector(CollectorConfig{
		NodeName: "test-node",
	})
	server := NewServer(ServerConfig{}, collector)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()

	server.handleStatus(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %v, want %v", resp.StatusCode, http.StatusOK)
	}

	var status NodeStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if status.Node.Name != "test-node" {
		t.Errorf("Node.Name = %v, want test-node", status.Node.Name)
	}
	if status.Version != StatusVersion {
		t.Errorf("Version = %v, want %v", status.Version, StatusVersion)
	}
}

func TestServerServicesEndpoint(t *testing.T) {
	collector := NewCollector(CollectorConfig{
		NodeName: "test-node",
	})
	server := NewServer(ServerConfig{}, collector)

	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	w := httptest.NewRecorder()

	server.handleServices(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %v, want %v", resp.StatusCode, http.StatusOK)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if _, ok := result["services"]; !ok {
		t.Error("Response should contain 'services' key")
	}
}

// freeTCPPort returns a currently-free localhost TCP port. NewServer treats
// Port==0 as "unset" and defaults it to 8080, so tests that actually Start() the
// server must pass a real ephemeral port — otherwise they bind :8080 and flake
// whenever anything else (e.g. the llamacpp container) holds it. See #405.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve an ephemeral port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func TestServerStartAndShutdown(t *testing.T) {
	collector := NewCollector(CollectorConfig{NodeName: "test"})
	server := NewServer(ServerConfig{Port: freeTCPPort(t)}, collector)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start(ctx)
	}()

	// Wait for context to cancel
	<-ctx.Done()

	// Server should shut down cleanly
	select {
	case err := <-errCh:
		if err != nil && err != context.DeadlineExceeded {
			t.Errorf("Start() error = %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("Server did not shut down in time")
	}
}

func TestDesktopEndpointsRegisteredWhenEnabled(t *testing.T) {
	collector := NewCollector(CollectorConfig{NodeName: "test"})
	validator := &mockTokenValidator{validToken: "valid-token"}

	server := NewServer(ServerConfig{
		Port:           0,
		TokenValidator: validator,
		OrgID:          "test-org",
		EnableDesktop:  true,
	}, collector)

	// Build the mux synchronously rather than starting the server in a
	// goroutine; this exercises the real route registration without racing
	// the server's startup goroutine over server.httpServer (issue #321).
	handler := server.buildMux()

	// Screenshot endpoint should return 401 without token (registered but auth fails)
	req := httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("/api/screenshot without token: got %d, want %d", w.Code, http.StatusUnauthorized)
	}

	// Actions endpoint should return 401 without token
	req = httptest.NewRequest(http.MethodPost, "/api/actions", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("/api/actions without token: got %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestDesktopEndpointsNotRegisteredWithoutEnableDesktop(t *testing.T) {
	collector := NewCollector(CollectorConfig{NodeName: "test"})
	validator := &mockTokenValidator{validToken: "valid-token"}

	// TokenValidator set but EnableDesktop is false -- endpoints should NOT be registered
	server := NewServer(ServerConfig{
		Port:           0,
		TokenValidator: validator,
		OrgID:          "test-org",
		EnableDesktop:  false,
	}, collector)

	// Build the mux synchronously to avoid racing the server startup
	// goroutine over server.httpServer (issue #321).
	handler := server.buildMux()

	// Screenshot endpoint should return 404 (not registered)
	req := httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("/api/screenshot with EnableDesktop=false: got %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestDesktopEndpointsNotRegisteredWithoutValidator(t *testing.T) {
	collector := NewCollector(CollectorConfig{NodeName: "test"})

	// EnableDesktop is true but no TokenValidator -- endpoints should NOT be registered
	server := NewServer(ServerConfig{
		Port:          0,
		EnableDesktop: true,
	}, collector)

	// Build the mux synchronously to avoid racing the server startup
	// goroutine over server.httpServer (issue #321).
	handler := server.buildMux()

	req := httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("/api/screenshot with no validator: got %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestDesktopEndpointRejectsInvalidToken(t *testing.T) {
	collector := NewCollector(CollectorConfig{NodeName: "test"})
	validator := &mockTokenValidator{validToken: "good-token"}

	server := NewServer(ServerConfig{
		Port:           0,
		TokenValidator: validator,
		OrgID:          "test-org",
		EnableDesktop:  true,
	}, collector)

	// Build the mux synchronously to avoid racing the server startup
	// goroutine over server.httpServer (issue #321).
	handler := server.buildMux()

	// Bad token should get 401
	req := httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("/api/screenshot with bad token: got %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestServerDualListen(t *testing.T) {
	collector := NewCollector(CollectorConfig{NodeName: "test-dual"})

	// Create an extra listener to simulate a VPN listener
	extraLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create extra listener: %v", err)
	}
	extraAddr := extraLn.Addr().(*net.TCPAddr)

	server := NewServer(ServerConfig{
		Port:    freeTCPPort(t),
		Version: "test-dual",
	}, collector)
	server.AddListener(extraLn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go server.Start(ctx)
	time.Sleep(100 * time.Millisecond) // Wait for server to start

	// The extra (VPN) listener should serve /health
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", extraAddr.Port))
	if err != nil {
		t.Fatalf("extra listener health check failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("extra listener: expected status 200, got %d", resp.StatusCode)
	}

	// The extra listener should serve /ping
	resp, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/ping", extraAddr.Port))
	if err != nil {
		t.Fatalf("extra listener ping failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("extra listener: expected ping status 200, got %d", resp.StatusCode)
	}
}

func TestExistingEndpointsUnaffectedByDesktopConfig(t *testing.T) {
	// Verify that /health, /status, /ping, /services continue to work
	// regardless of EnableDesktop setting
	collector := NewCollector(CollectorConfig{NodeName: "test"})

	for _, enableDesktop := range []bool{true, false} {
		t.Run(fmt.Sprintf("EnableDesktop=%v", enableDesktop), func(t *testing.T) {
			server := NewServer(ServerConfig{
				Port:          0,
				Version:       "1.0.0",
				EnableDesktop: enableDesktop,
			}, collector)

			// Build the mux synchronously to avoid racing the server startup
			// goroutine over server.httpServer (issue #321).
			handler := server.buildMux()

			// Health endpoint should always work
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("/health: got %d, want %d", w.Code, http.StatusOK)
			}

			// Ping endpoint should always work
			req = httptest.NewRequest(http.MethodGet, "/ping", nil)
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("/ping: got %d, want %d", w.Code, http.StatusOK)
			}
		})
	}
}
