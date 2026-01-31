package fabricserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewServer(t *testing.T) {
	s := NewServer(Config{
		Port:     8443,
		NodeName: "test-node",
	})

	if s.config.Port != 8443 {
		t.Errorf("Port = %d, want 8443", s.config.Port)
	}
	if s.config.NodeName != "test-node" {
		t.Errorf("NodeName = %s, want test-node", s.config.NodeName)
	}
}

func TestNewServerDefaults(t *testing.T) {
	s := NewServer(Config{})

	if s.config.Port != 8443 {
		t.Errorf("default Port = %d, want 8443", s.config.Port)
	}
	if s.config.ReadTimeout != 30_000_000_000 { // 30s in nanoseconds
		t.Errorf("default ReadTimeout = %v", s.config.ReadTimeout)
	}
}

func TestHealthEndpoint(t *testing.T) {
	s := NewServer(Config{NodeName: "test-node"})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
	if resp["node"] != "test-node" {
		t.Errorf("node = %v, want test-node", resp["node"])
	}
}

func TestListServicesEmpty(t *testing.T) {
	s := NewServer(Config{NodeName: "test-node"})

	req := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	w := httptest.NewRecorder()

	s.mux.ServeHTTP(w, req)

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	services := resp["services"].([]interface{})
	if len(services) != 0 {
		t.Errorf("services count = %d, want 0", len(services))
	}
}

func TestRegisterAndCallService(t *testing.T) {
	s := NewServer(Config{NodeName: "test-node"})

	// Register a test service
	s.RegisterService("echo", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"service": "echo",
			"path":    r.URL.Path,
		})
	})

	// Call it
	req := httptest.NewRequest(http.MethodPost, "/api/echo/test", nil)
	w := httptest.NewRecorder()

	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["service"] != "echo" {
		t.Errorf("service = %v, want echo", resp["service"])
	}
}

func TestServiceNotFound(t *testing.T) {
	s := NewServer(Config{NodeName: "test-node"})

	req := httptest.NewRequest(http.MethodGet, "/api/nonexistent/test", nil)
	w := httptest.NewRecorder()

	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestListServicesWithRegistered(t *testing.T) {
	s := NewServer(Config{NodeName: "test-node"})
	s.RegisterService("db", func(w http.ResponseWriter, r *http.Request) {})
	s.RegisterService("llm", func(w http.ResponseWriter, r *http.Request) {})

	req := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	w := httptest.NewRecorder()

	s.mux.ServeHTTP(w, req)

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	services := resp["services"].([]interface{})
	if len(services) != 2 {
		t.Errorf("services count = %d, want 2", len(services))
	}
}

func TestDetectVPNAddress(t *testing.T) {
	// This test validates the function runs without panicking.
	// On most dev machines, it will return an error (no VPN interface).
	_, err := detectVPNAddress()
	// We don't assert on the result since it depends on network config.
	_ = err
}
