// Package discovery provides a client for querying the AceTeam service discovery API.
//
// The discovery client queries the platform's /api/fabric/discover/nodes endpoint
// to find peer nodes by capability tags. This enables nodes to be aware of peers
// in the same organization and their capabilities.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Node represents a discovered peer node from the service discovery API.
type Node struct {
	ID           string   `json:"id"`
	Hostname     string   `json:"hostname"`
	GivenName    string   `json:"givenName"`
	Online       bool     `json:"online"`
	LastSeen     string   `json:"lastSeen"`
	IPAddresses  []string `json:"ipAddresses"`
	Tags         []string `json:"tags"`
	Capabilities []string `json:"capabilities"`
	Status       *Status  `json:"status,omitempty"`
}

// Status contains hardware metrics for a node.
type Status struct {
	CPUUsage       *float64 `json:"cpuUsage"`
	MemoryUsage    *float64 `json:"memoryUsage"`
	DiskUsage      *float64 `json:"diskUsage"`
	GPUUsage       *float64 `json:"gpuUsage"`
	GPUMemoryUsage *float64 `json:"gpuMemoryUsage"`
	GPUTemperature *float64 `json:"gpuTemperature"`
	IsOnline       bool     `json:"isOnline"`
	ReportedAt     string   `json:"reportedAt"`
}

// DiscoverResponse is the API response from the discover/nodes endpoint.
type DiscoverResponse struct {
	Nodes []Node `json:"nodes"`
	Total int    `json:"total"`
}

// Client queries the AceTeam service discovery API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client

	// Cached peer list
	mu         sync.RWMutex
	peers      []Node
	lastUpdate time.Time
	cacheTTL   time.Duration
}

// ClientConfig holds configuration for the discovery client.
type ClientConfig struct {
	// BaseURL is the AceTeam API base URL (e.g., "https://aceteam.ai")
	BaseURL string

	// APIKey is the authentication token
	APIKey string

	// CacheTTL is how long to cache the peer list (default: 60s)
	CacheTTL time.Duration

	// Timeout is the HTTP request timeout (default: 10s)
	Timeout time.Duration
}

// NewClient creates a new discovery client.
func NewClient(cfg ClientConfig) *Client {
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 60 * time.Second
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}

	return &Client{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
		cacheTTL: cfg.CacheTTL,
	}
}

// DiscoverNodes queries the service discovery API for nodes matching the given capabilities.
// If no capabilities are specified, returns all nodes in the organization.
func (c *Client) DiscoverNodes(ctx context.Context, capabilities []string, includeStatus bool, onlineOnly bool) ([]Node, error) {
	u, err := url.Parse(fmt.Sprintf("%s/api/fabric/discover/nodes", c.baseURL))
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	q := u.Query()
	for _, cap := range capabilities {
		q.Add("capability", cap)
	}
	if includeStatus {
		q.Set("includeStatus", "true")
	}
	if onlineOnly {
		q.Set("onlineOnly", "true")
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discovery request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery API returned status %d", resp.StatusCode)
	}

	var result DiscoverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.Nodes, nil
}

// GetPeers returns the cached peer list, refreshing if the cache has expired.
func (c *Client) GetPeers(ctx context.Context) ([]Node, error) {
	c.mu.RLock()
	if time.Since(c.lastUpdate) < c.cacheTTL && c.peers != nil {
		peers := c.peers
		c.mu.RUnlock()
		return peers, nil
	}
	c.mu.RUnlock()

	return c.RefreshPeers(ctx)
}

// RefreshPeers fetches the latest peer list from the discovery API.
func (c *Client) RefreshPeers(ctx context.Context) ([]Node, error) {
	nodes, err := c.DiscoverNodes(ctx, nil, false, false)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.peers = nodes
	c.lastUpdate = time.Now()
	c.mu.Unlock()

	return nodes, nil
}

// StartPeriodicRefresh begins a background goroutine that refreshes the peer cache.
// It runs until the context is cancelled.
func (c *Client) StartPeriodicRefresh(ctx context.Context) {
	ticker := time.NewTicker(c.cacheTTL)
	defer ticker.Stop()

	// Initial refresh
	if _, err := c.RefreshPeers(ctx); err != nil {
		fmt.Printf("   - Warning: Initial peer discovery failed: %v\n", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := c.RefreshPeers(ctx); err != nil {
				// Log but don't fail — peers are best-effort
				fmt.Printf("   - Warning: Peer discovery refresh failed: %v\n", err)
			}
		}
	}
}
