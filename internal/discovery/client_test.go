package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	c := NewClient(ClientConfig{
		BaseURL: "https://aceteam.ai",
		APIKey:  "test-key",
	})

	if c.baseURL != "https://aceteam.ai" {
		t.Errorf("baseURL = %v, want https://aceteam.ai", c.baseURL)
	}
	if c.cacheTTL != 60*time.Second {
		t.Errorf("cacheTTL = %v, want 60s", c.cacheTTL)
	}
}

func TestNewClientTrailingSlash(t *testing.T) {
	c := NewClient(ClientConfig{
		BaseURL: "https://aceteam.ai/",
		APIKey:  "test-key",
	})

	if c.baseURL != "https://aceteam.ai" {
		t.Errorf("baseURL = %v, want https://aceteam.ai (trailing slash stripped)", c.baseURL)
	}
}

func TestDiscoverNodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth header
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Verify path
		if r.URL.Path != "/api/fabric/discover/nodes" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// Verify query params
		caps := r.URL.Query()["capability"]
		if len(caps) != 2 || caps[0] != "gpu:a100" || caps[1] != "llm:llama3" {
			t.Errorf("unexpected capabilities: %v", caps)
		}

		if r.URL.Query().Get("onlineOnly") != "true" {
			t.Error("expected onlineOnly=true")
		}

		resp := DiscoverResponse{
			Nodes: []Node{
				{
					ID:           "1",
					Hostname:     "gpu-node-1",
					GivenName:    "gpu-node-1",
					Online:       true,
					IPAddresses:  []string{"100.64.0.1"},
					Tags:         []string{"gpu", "training"},
					Capabilities: []string{"gpu:a100", "llm:llama3", "vram:80gb"},
				},
			},
			Total: 1,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := NewClient(ClientConfig{
		BaseURL: server.URL,
		APIKey:  "test-key",
	})

	nodes, err := c.DiscoverNodes(context.Background(), []string{"gpu:a100", "llm:llama3"}, false, true)
	if err != nil {
		t.Fatalf("DiscoverNodes() error = %v", err)
	}

	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}

	if nodes[0].Hostname != "gpu-node-1" {
		t.Errorf("hostname = %v, want gpu-node-1", nodes[0].Hostname)
	}

	if len(nodes[0].Capabilities) != 3 {
		t.Errorf("capabilities count = %d, want 3", len(nodes[0].Capabilities))
	}
}

func TestDiscoverNodesError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := NewClient(ClientConfig{
		BaseURL: server.URL,
		APIKey:  "test-key",
	})

	_, err := c.DiscoverNodes(context.Background(), nil, false, false)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestGetPeersCaching(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := DiscoverResponse{
			Nodes: []Node{{ID: "1", Hostname: "node-1", Online: true}},
			Total: 1,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := NewClient(ClientConfig{
		BaseURL:  server.URL,
		APIKey:   "test-key",
		CacheTTL: 5 * time.Second, // Long cache
	})

	// First call — fetches from API
	peers, err := c.GetPeers(context.Background())
	if err != nil {
		t.Fatalf("GetPeers() error = %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(peers))
	}

	// Second call — should use cache
	peers, err = c.GetPeers(context.Background())
	if err != nil {
		t.Fatalf("GetPeers() error = %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(peers))
	}

	if callCount != 1 {
		t.Errorf("expected 1 API call (cached), got %d", callCount)
	}
}

func TestRefreshPeersInvalidatesCache(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := DiscoverResponse{
			Nodes: []Node{{ID: "1", Hostname: "node-1", Online: true}},
			Total: 1,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := NewClient(ClientConfig{
		BaseURL:  server.URL,
		APIKey:   "test-key",
		CacheTTL: 5 * time.Second,
	})

	// First call
	c.GetPeers(context.Background())

	// Force refresh
	c.RefreshPeers(context.Background())

	if callCount != 2 {
		t.Errorf("expected 2 API calls after refresh, got %d", callCount)
	}
}
