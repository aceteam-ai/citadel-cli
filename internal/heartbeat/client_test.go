package heartbeat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

func TestNewClient(t *testing.T) {
	collector := status.NewCollector(status.CollectorConfig{
		NodeName: "test-node",
	})

	tests := []struct {
		name         string
		config       ClientConfig
		wantEndpoint string
		wantInterval time.Duration
	}{
		{
			name: "with defaults",
			config: ClientConfig{
				BaseURL: "https://aceteam.ai",
				NodeID:  "node-123",
			},
			wantEndpoint: "https://aceteam.ai/api/fabric/nodes/node-123/heartbeat",
			wantInterval: 30 * time.Second,
		},
		{
			name: "with custom interval",
			config: ClientConfig{
				BaseURL:  "https://api.example.com",
				NodeID:   "my-node",
				Interval: 60 * time.Second,
			},
			wantEndpoint: "https://api.example.com/api/fabric/nodes/my-node/heartbeat",
			wantInterval: 60 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.config, collector)

			if client == nil {
				t.Fatal("NewClient returned nil")
			}
			if client.Endpoint() != tt.wantEndpoint {
				t.Errorf("Endpoint() = %v, want %v", client.Endpoint(), tt.wantEndpoint)
			}
			if client.Interval() != tt.wantInterval {
				t.Errorf("Interval() = %v, want %v", client.Interval(), tt.wantInterval)
			}
		})
	}
}

func TestClientEndpoint(t *testing.T) {
	collector := status.NewCollector(status.CollectorConfig{NodeName: "test"})
	client := NewClient(ClientConfig{
		BaseURL: "https://aceteam.ai",
		NodeID:  "test-node-456",
	}, collector)

	expected := "https://aceteam.ai/api/fabric/nodes/test-node-456/heartbeat"
	if client.Endpoint() != expected {
		t.Errorf("Endpoint() = %v, want %v", client.Endpoint(), expected)
	}
}

func TestClientInterval(t *testing.T) {
	collector := status.NewCollector(status.CollectorConfig{NodeName: "test"})
	client := NewClient(ClientConfig{
		BaseURL:  "https://aceteam.ai",
		NodeID:   "test",
		Interval: 45 * time.Second,
	}, collector)

	if client.Interval() != 45*time.Second {
		t.Errorf("Interval() = %v, want 45s", client.Interval())
	}
}

func TestClientSendOnce(t *testing.T) {
	// Create test server
	var receivedPayload map[string]any
	var receivedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()

		if r.Method != http.MethodPost {
			t.Errorf("Method = %v, want POST", r.Method)
		}

		if err := json.NewDecoder(r.Body).Decode(&receivedPayload); err != nil {
			t.Errorf("Failed to decode request body: %v", err)
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	collector := status.NewCollector(status.CollectorConfig{
		NodeName: "test-node",
	})

	client := NewClient(ClientConfig{
		BaseURL: server.URL,
		NodeID:  "test-node-123",
		APIKey:  "test-api-key",
	}, collector)

	ctx := context.Background()
	err := client.SendOnce(ctx)

	if err != nil {
		t.Errorf("SendOnce() error = %v, want nil", err)
	}

	// Check headers
	if receivedHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %v, want application/json", receivedHeaders.Get("Content-Type"))
	}
	if receivedHeaders.Get("Authorization") != "Bearer test-api-key" {
		t.Errorf("Authorization = %v, want 'Bearer test-api-key'", receivedHeaders.Get("Authorization"))
	}
	if receivedHeaders.Get("X-Citadel-Node-ID") != "test-node-123" {
		t.Errorf("X-Citadel-Node-ID = %v, want test-node-123", receivedHeaders.Get("X-Citadel-Node-ID"))
	}

	// Check payload has expected fields
	if receivedPayload == nil {
		t.Fatal("Received payload is nil")
	}
	if receivedPayload["version"] == nil {
		t.Error("Payload should have 'version' field")
	}
}

func TestClientSendOnceError(t *testing.T) {
	// Create test server that returns error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	collector := status.NewCollector(status.CollectorConfig{NodeName: "test"})
	client := NewClient(ClientConfig{
		BaseURL: server.URL,
		NodeID:  "test",
	}, collector)

	ctx := context.Background()
	err := client.SendOnce(ctx)

	if err == nil {
		t.Error("SendOnce() should return error for 500 response")
	}
}

func TestClientStartAndStop(t *testing.T) {
	// Create test server that counts requests
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	collector := status.NewCollector(status.CollectorConfig{NodeName: "test"})
	client := NewClient(ClientConfig{
		BaseURL:  server.URL,
		NodeID:   "test",
		Interval: 100 * time.Millisecond, // Interval for testing
		Timeout:  500 * time.Millisecond, // Give requests enough time
	}, collector)

	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()

	err := client.Start(ctx)

	// Should return context error after timeout
	if err != context.DeadlineExceeded {
		t.Errorf("Start() error = %v, want context.DeadlineExceeded", err)
	}

	// Should have made at least 1 request (initial heartbeat)
	// Note: Due to timing, we might get 1-3 requests depending on system load
	if requestCount < 1 {
		t.Errorf("Request count = %d, want >= 1", requestCount)
	}
}

func TestClientNoAPIKey(t *testing.T) {
	var receivedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	collector := status.NewCollector(status.CollectorConfig{NodeName: "test"})
	client := NewClient(ClientConfig{
		BaseURL: server.URL,
		NodeID:  "test",
		// No APIKey
	}, collector)

	ctx := context.Background()
	client.SendOnce(ctx)

	// Should not have Authorization header
	if receivedHeaders.Get("Authorization") != "" {
		t.Errorf("Authorization should be empty when no API key, got %v", receivedHeaders.Get("Authorization"))
	}
}

func TestClientConfigDefaults(t *testing.T) {
	collector := status.NewCollector(status.CollectorConfig{NodeName: "test"})
	client := NewClient(ClientConfig{
		BaseURL: "https://example.com",
		NodeID:  "node-1",
		// All other fields use defaults
	}, collector)

	if client.Interval() != 30*time.Second {
		t.Errorf("Default interval = %v, want 30s", client.Interval())
	}
}
