// internal/network/server.go
// Core tsnet wrapper for AceTeam Network connections
package network

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"tailscale.com/client/tailscale"
	"tailscale.com/tsnet"
)

// NetworkServer wraps tsnet.Server with AceTeam-specific functionality.
type NetworkServer struct {
	srv        *tsnet.Server
	controlURL string
	hostname   string
	stateDir   string

	mu        sync.RWMutex
	connected bool
}

// ServerConfig holds configuration for creating a NetworkServer.
type ServerConfig struct {
	// Hostname is the name this node will have on the network
	Hostname string

	// ControlURL is the Headscale/Nexus coordination server URL
	ControlURL string

	// StateDir is where tsnet stores its state (keys, etc.)
	// If empty, uses GetStateDir()
	StateDir string

	// Ephemeral makes this node ephemeral (removed when disconnected)
	Ephemeral bool

	// AuthKey is a pre-authorized key for non-interactive login
	AuthKey string
}

// NewServer creates a new NetworkServer with the given configuration.
// The server is not connected until Connect() is called.
func NewServer(config ServerConfig) *NetworkServer {
	stateDir := config.StateDir
	if stateDir == "" {
		stateDir = GetStateDir()
	}

	return &NetworkServer{
		controlURL: config.ControlURL,
		hostname:   config.Hostname,
		stateDir:   stateDir,
	}
}

// Connect establishes the network connection.
// If authKey is provided, it's used for authentication.
// Otherwise, interactive authentication is required (device auth flow).
func (s *NetworkServer) Connect(ctx context.Context, authKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.connected && s.srv != nil {
		return nil // Already connected
	}

	// Ensure state directory exists
	if _, err := EnsureStateDir(); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	// Create tsnet server
	s.srv = &tsnet.Server{
		Hostname:   s.hostname,
		ControlURL: s.controlURL,
		Dir:        s.stateDir,
		AuthKey:    authKey,
		Ephemeral:  false, // We want persistent nodes
	}

	// Start the server (this initiates the connection)
	if err := s.srv.Start(); err != nil {
		return fmt.Errorf("failed to start network: %w", err)
	}

	// Wait for connection to be established
	if err := s.waitForConnection(ctx); err != nil {
		s.srv.Close()
		s.srv = nil
		return err
	}

	s.connected = true
	return nil
}

// waitForConnection waits for the network connection to be established.
func (s *NetworkServer) waitForConnection(ctx context.Context) error {
	// Create a timeout context if one isn't already set
	timeoutCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	lc, err := s.srv.LocalClient()
	if err != nil {
		return fmt.Errorf("failed to get local client: %w", err)
	}

	// Poll for connection status
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timeout waiting for network connection")
		case <-ticker.C:
			status, err := lc.Status(timeoutCtx)
			if err != nil {
				continue // Keep trying
			}
			if status.BackendState == "Running" {
				return nil
			}
		}
	}
}

// Disconnect closes the network connection.
func (s *NetworkServer) Disconnect() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.srv == nil {
		return nil
	}

	err := s.srv.Close()
	s.srv = nil
	s.connected = false
	return err
}

// IsConnected returns true if connected to the network.
func (s *NetworkServer) IsConnected() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.connected || s.srv == nil {
		return false
	}

	// Verify actual connection status
	lc, err := s.srv.LocalClient()
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status, err := lc.Status(ctx)
	if err != nil {
		return false
	}

	return status.BackendState == "Running"
}

// GetIPv4 returns the node's IPv4 address on the network.
func (s *NetworkServer) GetIPv4() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.srv == nil {
		return "", fmt.Errorf("not connected to network")
	}

	ip4, _ := s.srv.TailscaleIPs()
	if !ip4.IsValid() {
		return "", fmt.Errorf("no IPv4 address assigned")
	}

	return ip4.String(), nil
}

// GetIPv6 returns the node's IPv6 address on the network.
func (s *NetworkServer) GetIPv6() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.srv == nil {
		return "", fmt.Errorf("not connected to network")
	}

	_, ip6 := s.srv.TailscaleIPs()
	if !ip6.IsValid() {
		return "", fmt.Errorf("no IPv6 address assigned")
	}

	return ip6.String(), nil
}

// GetIPs returns all network IPs assigned to this node.
func (s *NetworkServer) GetIPs() ([]netip.Addr, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.srv == nil {
		return nil, fmt.Errorf("not connected to network")
	}

	ip4, ip6 := s.srv.TailscaleIPs()
	var ips []netip.Addr
	if ip4.IsValid() {
		ips = append(ips, ip4)
	}
	if ip6.IsValid() {
		ips = append(ips, ip6)
	}
	return ips, nil
}

// Hostname returns the configured hostname.
func (s *NetworkServer) Hostname() string {
	return s.hostname
}

// LocalClient returns the tailscale LocalClient for advanced operations.
func (s *NetworkServer) LocalClient() (*tailscale.LocalClient, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.srv == nil {
		return nil, fmt.Errorf("not connected to network")
	}

	return s.srv.LocalClient()
}

// Listen creates a listener on the network for the given address.
// This allows exposing services to the AceTeam network.
func (s *NetworkServer) Listen(network, addr string) (net.Listener, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.srv == nil {
		return nil, fmt.Errorf("not connected to network")
	}

	return s.srv.Listen(network, addr)
}

// ListenTLS creates a TLS listener with automatic certificate management.
func (s *NetworkServer) ListenTLS(network, addr string) (net.Listener, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.srv == nil {
		return nil, fmt.Errorf("not connected to network")
	}

	return s.srv.ListenTLS(network, addr)
}

// Dial connects to a remote address on the network.
func (s *NetworkServer) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.srv == nil {
		return nil, fmt.Errorf("not connected to network")
	}

	return s.srv.Dial(ctx, network, addr)
}

// Status returns the current network status.
func (s *NetworkServer) Status(ctx context.Context) (*NetworkStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.srv == nil {
		return &NetworkStatus{
			Connected: false,
		}, nil
	}

	lc, err := s.srv.LocalClient()
	if err != nil {
		return nil, fmt.Errorf("failed to get local client: %w", err)
	}

	status, err := lc.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get status: %w", err)
	}

	ip4, ip6 := s.srv.TailscaleIPs()
	var ipv4, ipv6 string
	if ip4.IsValid() {
		ipv4 = ip4.String()
	}
	if ip6.IsValid() {
		ipv6 = ip6.String()
	}

	return &NetworkStatus{
		Connected:    status.BackendState == "Running",
		BackendState: status.BackendState,
		Hostname:     s.hostname,
		IPv4:         ipv4,
		IPv6:         ipv6,
		ControlURL:   s.controlURL,
	}, nil
}

// NetworkStatus represents the current network connection status.
type NetworkStatus struct {
	Connected    bool   `json:"connected"`
	BackendState string `json:"backend_state"`
	Hostname     string `json:"hostname"`
	IPv4         string `json:"ipv4,omitempty"`
	IPv6         string `json:"ipv6,omitempty"`
	ControlURL   string `json:"control_url"`
}
