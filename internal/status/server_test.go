package status

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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

func TestServerStartAndShutdown(t *testing.T) {
	collector := NewCollector(CollectorConfig{NodeName: "test"})
	server := NewServer(ServerConfig{Port: 0}, collector) // Port 0 for random available port

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
