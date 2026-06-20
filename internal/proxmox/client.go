// Package proxmox provides an HTTP API client for Proxmox VE.
//
// It supports both API token authentication (preferred) and ticket-based
// authentication. TLS verification is disabled by default since most
// Proxmox installs use self-signed certificates.
package proxmox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client communicates with a Proxmox VE API server.
type Client struct {
	baseURL    string // e.g. "https://192.168.2.4:8006"
	httpClient *http.Client

	// Auth: exactly one of tokenHeader or ticket should be set.
	tokenHeader string // "PVEAPIToken=user@realm!tokenid=secret"
	ticket      string // CSRFPreventionToken-based session
	csrfToken   string
}

// BaseURL returns the configured Proxmox API base URL (e.g. "https://192.168.2.4:8006").
func (c *Client) BaseURL() string {
	return c.baseURL
}

// ClientConfig holds configuration for creating a new Client.
type ClientConfig struct {
	// BaseURL is the Proxmox host URL, e.g. "https://192.168.2.4:8006".
	BaseURL string

	// TokenID is the API token in the format "user@realm!tokenid".
	TokenID string

	// TokenSecret is the API token secret (UUID).
	TokenSecret string

	// Username and Password for ticket-based auth (used if TokenID is empty).
	Username string
	Password string

	// InsecureSkipVerify disables TLS certificate verification.
	// Default: true (most Proxmox installs use self-signed certs).
	InsecureSkipVerify *bool

	// Timeout for HTTP requests. Default: 15s.
	Timeout time.Duration
}

// NewClient creates a Proxmox API client from the given configuration.
func NewClient(cfg ClientConfig) *Client {
	skipVerify := true
	if cfg.InsecureSkipVerify != nil {
		skipVerify = *cfg.InsecureSkipVerify
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}

	c := &Client{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: skipVerify,
				},
			},
		},
	}

	if cfg.TokenID != "" && cfg.TokenSecret != "" {
		c.tokenHeader = fmt.Sprintf("PVEAPIToken=%s=%s", cfg.TokenID, cfg.TokenSecret)
	}

	return c
}

// Authenticate performs ticket-based authentication and stores the session
// ticket and CSRF token. Only needed when not using API token auth.
func (c *Client) Authenticate(ctx context.Context, username, password string) error {
	form := url.Values{
		"username": {username},
		"password": {password},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api2/json/access/ticket",
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("creating auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("auth request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("auth failed (status %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data struct {
			Ticket              string `json:"ticket"`
			CSRFPreventionToken string `json:"CSRFPreventionToken"`
			Username            string `json:"username"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decoding auth response: %w", err)
	}

	c.ticket = result.Data.Ticket
	c.csrfToken = result.Data.CSRFPreventionToken
	return nil
}

// IsAuthenticated returns true if the client has valid credentials configured.
func (c *Client) IsAuthenticated() bool {
	return c.tokenHeader != "" || c.ticket != ""
}

// apiResponse is the common Proxmox API response envelope.
type apiResponse struct {
	Data json.RawMessage `json:"data"`
}

// get performs an authenticated GET request and returns the "data" field.
func (c *Client) get(ctx context.Context, path string) (json.RawMessage, error) {
	return c.doRequest(ctx, http.MethodGet, path, nil)
}

// post performs an authenticated POST request with form data.
func (c *Client) post(ctx context.Context, path string, form url.Values) (json.RawMessage, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	return c.doRequest(ctx, http.MethodPost, path, body)
}

func (c *Client) doRequest(ctx context.Context, method, path string, body io.Reader) (json.RawMessage, error) {
	fullURL := c.baseURL + "/api2/json" + path

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	// Set auth headers
	if c.tokenHeader != "" {
		req.Header.Set("Authorization", c.tokenHeader)
	} else if c.ticket != "" {
		req.AddCookie(&http.Cookie{Name: "PVEAuthCookie", Value: c.ticket})
		if method != http.MethodGet && c.csrfToken != "" {
			req.Header.Set("CSRFPreventionToken", c.csrfToken)
		}
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var envelope apiResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return envelope.Data, nil
}

// Node represents a Proxmox cluster node.
type Node struct {
	Node   string  `json:"node"`
	Status string  `json:"status"`
	CPU    float64 `json:"cpu"`
	MaxCPU int     `json:"maxcpu"`
	Mem    int64   `json:"mem"`
	MaxMem int64   `json:"maxmem"`
	Uptime int64   `json:"uptime"`
}

// ListNodes returns all nodes in the Proxmox cluster.
func (c *Client) ListNodes(ctx context.Context) ([]Node, error) {
	data, err := c.get(ctx, "/nodes")
	if err != nil {
		return nil, err
	}
	var nodes []Node
	if err := json.Unmarshal(data, &nodes); err != nil {
		return nil, fmt.Errorf("parsing nodes: %w", err)
	}
	return nodes, nil
}

// Guest represents a VM (QEMU) or container (LXC) on a Proxmox node.
type Guest struct {
	VMID     int     `json:"vmid"`
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	Type     string  `json:"type"`
	CPU      float64 `json:"cpu"`
	CPUs     int     `json:"cpus"`
	Mem      int64   `json:"mem"`
	MaxMem   int64   `json:"maxmem"`
	Disk     int64   `json:"disk"`
	MaxDisk  int64   `json:"maxdisk"`
	Uptime   int64   `json:"uptime"`
	NetIn    int64   `json:"netin"`
	NetOut   int64   `json:"netout"`
	PID      int     `json:"pid"`
	Template int     `json:"template"`
	Tags     string  `json:"tags"`
	Lock     string  `json:"lock"`
}

// ListVMs returns all QEMU VMs on the given node.
func (c *Client) ListVMs(ctx context.Context, node string) ([]Guest, error) {
	data, err := c.get(ctx, fmt.Sprintf("/nodes/%s/qemu", node))
	if err != nil {
		return nil, err
	}
	var guests []Guest
	if err := json.Unmarshal(data, &guests); err != nil {
		return nil, fmt.Errorf("parsing VMs: %w", err)
	}
	for i := range guests {
		guests[i].Type = "qemu"
	}
	return guests, nil
}

// ListContainers returns all LXC containers on the given node.
func (c *Client) ListContainers(ctx context.Context, node string) ([]Guest, error) {
	data, err := c.get(ctx, fmt.Sprintf("/nodes/%s/lxc", node))
	if err != nil {
		return nil, err
	}
	var guests []Guest
	if err := json.Unmarshal(data, &guests); err != nil {
		return nil, fmt.Errorf("parsing containers: %w", err)
	}
	for i := range guests {
		guests[i].Type = "lxc"
	}
	return guests, nil
}

// ListAllGuests returns all VMs and containers on the given node, merged.
func (c *Client) ListAllGuests(ctx context.Context, node string) ([]Guest, error) {
	vms, err := c.ListVMs(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("listing VMs: %w", err)
	}
	cts, err := c.ListContainers(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	return append(vms, cts...), nil
}

// GuestStatus holds the detailed status of a single VM/container.
type GuestStatus struct {
	Status    string  `json:"status"`
	VMID      int     `json:"vmid"`
	Name      string  `json:"name"`
	CPU       float64 `json:"cpu"`
	CPUs      int     `json:"cpus"`
	Mem       int64   `json:"mem"`
	MaxMem    int64   `json:"maxmem"`
	Uptime    int64   `json:"uptime"`
	PID       int     `json:"pid"`
	QMPStatus string  `json:"qmpstatus,omitempty"`
}

// GetGuestStatus returns the current status of a VM or container.
func (c *Client) GetGuestStatus(ctx context.Context, node, guestType string, vmid int) (*GuestStatus, error) {
	path := fmt.Sprintf("/nodes/%s/%s/%d/status/current", node, guestType, vmid)
	data, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	var status GuestStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, fmt.Errorf("parsing guest status: %w", err)
	}
	return &status, nil
}

// StartGuest starts a stopped VM or container.
func (c *Client) StartGuest(ctx context.Context, node, guestType string, vmid int) error {
	path := fmt.Sprintf("/nodes/%s/%s/%d/status/start", node, guestType, vmid)
	_, err := c.post(ctx, path, nil)
	return err
}

// StopGuest force-stops a VM or container.
func (c *Client) StopGuest(ctx context.Context, node, guestType string, vmid int) error {
	path := fmt.Sprintf("/nodes/%s/%s/%d/status/stop", node, guestType, vmid)
	_, err := c.post(ctx, path, nil)
	return err
}

// ShutdownGuest performs a graceful shutdown (ACPI) of a VM or container.
func (c *Client) ShutdownGuest(ctx context.Context, node, guestType string, vmid int) error {
	path := fmt.Sprintf("/nodes/%s/%s/%d/status/shutdown", node, guestType, vmid)
	_, err := c.post(ctx, path, nil)
	return err
}

// RebootGuest reboots a VM or container.
func (c *Client) RebootGuest(ctx context.Context, node, guestType string, vmid int) error {
	path := fmt.Sprintf("/nodes/%s/%s/%d/status/reboot", node, guestType, vmid)
	_, err := c.post(ctx, path, nil)
	return err
}

// GetGuestConfig returns the configuration of a VM or container as raw JSON.
func (c *Client) GetGuestConfig(ctx context.Context, node, guestType string, vmid int) (json.RawMessage, error) {
	path := fmt.Sprintf("/nodes/%s/%s/%d/config", node, guestType, vmid)
	return c.get(ctx, path)
}

// StoragePool represents a storage pool on a node.
type StoragePool struct {
	Storage string `json:"storage"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Active  int    `json:"active"`
	Enabled int    `json:"enabled"`
	Shared  int    `json:"shared"`
	Total   int64  `json:"total"`
	Used    int64  `json:"used"`
	Avail   int64  `json:"avail"`
}

// ListStorage returns storage pools on the given node.
func (c *Client) ListStorage(ctx context.Context, node string) ([]StoragePool, error) {
	data, err := c.get(ctx, fmt.Sprintf("/nodes/%s/storage", node))
	if err != nil {
		return nil, err
	}
	var pools []StoragePool
	if err := json.Unmarshal(data, &pools); err != nil {
		return nil, fmt.Errorf("parsing storage: %w", err)
	}
	return pools, nil
}

// ClusterResource represents a resource entry from the cluster-wide view.
type ClusterResource struct {
	ID      string  `json:"id"`
	Type    string  `json:"type"`
	Node    string  `json:"node"`
	Status  string  `json:"status"`
	Name    string  `json:"name"`
	VMID    int     `json:"vmid,omitempty"`
	CPU     float64 `json:"cpu"`
	MaxCPU  int     `json:"maxcpu"`
	Mem     int64   `json:"mem"`
	MaxMem  int64   `json:"maxmem"`
	Disk    int64   `json:"disk"`
	MaxDisk int64   `json:"maxdisk"`
	Uptime  int64   `json:"uptime"`
}

// ListClusterResources returns all resources across the cluster.
func (c *Client) ListClusterResources(ctx context.Context) ([]ClusterResource, error) {
	data, err := c.get(ctx, "/cluster/resources")
	if err != nil {
		return nil, err
	}
	var resources []ClusterResource
	if err := json.Unmarshal(data, &resources); err != nil {
		return nil, fmt.Errorf("parsing cluster resources: %w", err)
	}
	return resources, nil
}

// Ping checks connectivity to the Proxmox API by fetching the version.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.get(ctx, "/version")
	return err
}
