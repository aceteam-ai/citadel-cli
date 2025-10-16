// internal/nexus/client.go
package nexus

import (
	"fmt"
	"net/http"
	"time"
)

// Node represents a single compute node in the Nexus.
type Node struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	IPAddress string    `json:"ip_address"`
	Status    string    `json:"status"` // e.g., "online", "offline"
	LastSeen  time.Time `json:"last_seen"`
}

// Client is a client for interacting with the Nexus API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new Nexus API client.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// ListNodes fetches all nodes from the Nexus.
// For now, this returns mock data. We will replace this with a real HTTP call.
func (c *Client) ListNodes() ([]Node, error) {
	// --- MOCK IMPLEMENTATION ---
	// In the future, this will make an HTTP GET request to c.baseURL + "/api/v1/nodes"
	// and handle authentication (likely via Tailscale's identity headers).
	fmt.Println("[DEBUG] NexusClient: Using mock data for ListNodes()")
	mockNodes := []Node{
		{
			ID:        "node-123",
			Name:      "gpu-rig-01",
			IPAddress: "100.64.0.1",
			Status:    "online",
			LastSeen:  time.Now().Add(-1 * time.Minute),
		},
		{
			ID:        "node-456",
			Name:      "cpu-builder",
			IPAddress: "100.64.0.2",
			Status:    "offline",
			LastSeen:  time.Now().Add(-2 * time.Hour),
		},
	}
	return mockNodes, nil
	// --- END MOCK IMPLEMENTATION ---
}
