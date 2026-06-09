package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewServer_Defaults(t *testing.T) {
	s := NewServer(Config{})
	if s.config.Port != 8443 {
		t.Errorf("default port = %d, want 8443", s.config.Port)
	}
	if s.config.Upstreams == nil {
		t.Error("upstreams map should be initialized")
	}
}

func TestNewServer_CustomPort(t *testing.T) {
	s := NewServer(Config{Port: 9443})
	if s.config.Port != 9443 {
		t.Errorf("port = %d, want 9443", s.config.Port)
	}
}

func TestHandleRoot_Exact(t *testing.T) {
	s := NewServer(Config{NodeName: "test-node"})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	s.handleRoot(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["gateway"] != "citadel" {
		t.Errorf("gateway = %v, want citadel", resp["gateway"])
	}
	if resp["node"] != "test-node" {
		t.Errorf("node = %v, want test-node", resp["node"])
	}
}

func TestHandleRoot_NotFound(t *testing.T) {
	s := NewServer(Config{NodeName: "test-node"})

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	w := httptest.NewRecorder()
	s.handleRoot(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestAddUpstream(t *testing.T) {
	s := NewServer(Config{})
	s.AddUpstream("/health", &Upstream{Address: "127.0.0.1:8080"})
	s.AddUpstream("/api", &Upstream{Address: "127.0.0.1:8443", StripPrefix: true})

	if len(s.config.Upstreams) != 2 {
		t.Errorf("upstream count = %d, want 2", len(s.config.Upstreams))
	}

	if u := s.config.Upstreams["/health"]; u.Address != "127.0.0.1:8080" {
		t.Errorf("health upstream = %s, want 127.0.0.1:8080", u.Address)
	}
}

func TestIsWebSocketUpgrade(t *testing.T) {
	tests := []struct {
		name       string
		connection string
		upgrade    string
		want       bool
	}{
		{"valid ws", "upgrade", "websocket", true},
		{"valid ws mixed case", "Upgrade", "WebSocket", true},
		{"no upgrade header", "", "websocket", false},
		{"no websocket", "upgrade", "", false},
		{"normal request", "", "", false},
		{"wrong upgrade type", "upgrade", "h2c", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.connection != "" {
				req.Header.Set("Connection", tt.connection)
			}
			if tt.upgrade != "" {
				req.Header.Set("Upgrade", tt.upgrade)
			}

			got := isWebSocketUpgrade(req)
			if got != tt.want {
				t.Errorf("isWebSocketUpgrade() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProxyRouting(t *testing.T) {
	// Two separate backends to verify route discrimination
	statusBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"backend":     "status",
			"path":        r.URL.Path,
			"x_node":      r.Header.Get("X-Citadel-Node"),
			"x_fwd_proto": r.Header.Get("X-Forwarded-Proto"),
		})
	}))
	defer statusBackend.Close()

	vncBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"backend": "vnc",
			"path":    r.URL.Path,
		})
	}))
	defer vncBackend.Close()

	statusAddr := statusBackend.URL[len("http://"):]
	vncAddr := vncBackend.URL[len("http://"):]

	// Create gateway with upstreams on separate backends
	gw := NewServer(Config{
		Port:     0,
		NodeName: "test-node",
	})
	gw.AddUpstream("/health", &Upstream{Address: statusAddr})
	gw.AddUpstream("/api/screenshot", &Upstream{Address: statusAddr})
	gw.AddUpstream("/api/actions", &Upstream{Address: statusAddr})
	gw.AddUpstream("/vnc", &Upstream{Address: vncAddr, StripPrefix: true, WebSocket: true})

	// Build routes
	for prefix, upstream := range gw.config.Upstreams {
		gw.registerProxy(prefix, upstream)
	}
	gw.mux.HandleFunc("/", gw.handleRoot)

	// Test health goes to status backend
	t.Run("health -> status backend", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		w := httptest.NewRecorder()
		gw.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}

		var resp map[string]string
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["backend"] != "status" {
			t.Errorf("routed to %q backend, want status", resp["backend"])
		}
		if resp["path"] != "/health" {
			t.Errorf("proxied path = %q, want /health", resp["path"])
		}
		if resp["x_node"] != "test-node" {
			t.Errorf("X-Citadel-Node = %q, want test-node", resp["x_node"])
		}
		if resp["x_fwd_proto"] != "https" {
			t.Errorf("X-Forwarded-Proto = %q, want https", resp["x_fwd_proto"])
		}
	})

	// Test /api/screenshot goes to status backend
	t.Run("api/screenshot -> status backend", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
		w := httptest.NewRecorder()
		gw.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}

		var resp map[string]string
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["backend"] != "status" {
			t.Errorf("routed to %q backend, want status", resp["backend"])
		}
	})

	// Test /vnc goes to vnc backend with strip prefix
	t.Run("vnc -> vnc backend with strip", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/vnc/info", nil)
		w := httptest.NewRecorder()
		gw.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}

		var resp map[string]string
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["backend"] != "vnc" {
			t.Errorf("routed to %q backend, want vnc", resp["backend"])
		}
		if resp["path"] != "/info" {
			t.Errorf("proxied path = %q, want /info (strip prefix)", resp["path"])
		}
	})

	// Test root returns gateway info (not proxied)
	t.Run("root info", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		gw.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}

		var resp map[string]interface{}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["gateway"] != "citadel" {
			t.Errorf("gateway = %v, want citadel", resp["gateway"])
		}
	})
}

func TestProxyUpstreamDown(t *testing.T) {
	// Use a port that nothing is listening on
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := listener.Addr().String()
	listener.Close() // Close it so it's unreachable

	gw := NewServer(Config{NodeName: "test-node"})
	gw.AddUpstream("/dead", &Upstream{Address: deadAddr})

	for prefix, upstream := range gw.config.Upstreams {
		gw.registerProxy(prefix, upstream)
	}

	req := httptest.NewRequest(http.MethodGet, "/dead", nil)
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestStartAndShutdown(t *testing.T) {
	// Find a free port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	gw := NewServer(Config{
		Port:          port,
		ListenAddress: fmt.Sprintf("127.0.0.1:%d", port),
		NodeName:      "test-node",
		// No TLS — plain HTTP for testing
	})

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- gw.Start(ctx)
	}()

	// Give it time to start
	time.Sleep(100 * time.Millisecond)

	// Make a request
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200, body: %s", resp.StatusCode, string(body))
	}

	// Shutdown
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start() returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Start() did not return after context cancel")
	}
}
