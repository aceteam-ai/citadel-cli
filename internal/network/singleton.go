// internal/network/singleton.go
// Global NetworkServer instance management
package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrStaleState is returned when network state exists but the connection
// cannot be re-established (e.g. expired/revoked Headscale preauth key).
// Callers should clear state and re-authenticate with a fresh authkey.
var ErrStaleState = errors.New("network state is stale: connection cannot be re-established with existing keys")

// reconnectTimeout is the maximum time to wait for a single reconnection
// attempt using existing state before declaring it timed out. A working
// WireGuard handshake completes in under 5s; 10s gives margin for slow
// networks without making interactive login feel stuck.
const reconnectTimeout = 10 * time.Second

// reconnectAttempts is the number of times VerifyOrReconnect tries the
// no-authkey reconnect before declaring the state stale. On boot the network
// interface may not be ready within the first 10s timeout, so retrying avoids
// a premature ClearState + fresh connect that mints a new Headscale node ID
// (issue #246).
const reconnectAttempts = 3

var (
	globalServer *NetworkServer
	globalMu     sync.RWMutex

	// DefaultControlURL is the default Nexus coordination server
	DefaultControlURL = "https://nexus.aceteam.ai"
)

// Global returns the global NetworkServer instance.
// Returns nil if not initialized.
func Global() *NetworkServer {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return globalServer
}

// SetGlobal sets the global NetworkServer instance.
func SetGlobal(s *NetworkServer) {
	globalMu.Lock()
	defer globalMu.Unlock()
	globalServer = s
}

// ClearGlobal clears the global NetworkServer instance.
func ClearGlobal() {
	globalMu.Lock()
	defer globalMu.Unlock()
	globalServer = nil
}

// IsGlobalConnected returns true if the global server exists and is connected.
func IsGlobalConnected() bool {
	s := Global()
	if s == nil {
		return false
	}
	return s.IsConnected()
}

// EnsureConnected ensures the global server is connected.
// If already connected, returns the existing server.
// If state exists but not connected, reconnects.
// If no state exists, returns an error (must use Connect first).
func EnsureConnected(ctx context.Context, controlURL, hostname string) (*NetworkServer, error) {
	// Check if already connected
	s := Global()
	if s != nil && s.IsConnected() {
		return s, nil
	}

	// Check if we have existing state
	if !HasState() {
		return nil, fmt.Errorf("not logged in to AceTeam Network (run 'citadel login' first)")
	}

	// Reconnect using saved state
	config := ServerConfig{
		Hostname:   hostname,
		ControlURL: controlURL,
		StateDir:   GetStateDir(),
	}

	srv := NewServer(config)
	if err := srv.Connect(ctx, ""); err != nil {
		return nil, fmt.Errorf("failed to reconnect: %w", err)
	}

	SetGlobal(srv)
	return srv, nil
}

// Connect creates a new connection to the AceTeam Network.
// This is the primary way to establish a new connection.
func Connect(ctx context.Context, config ServerConfig) (*NetworkServer, error) {
	// Check if already connected
	s := Global()
	if s != nil && s.IsConnected() {
		return s, nil
	}

	srv := NewServer(config)
	if err := srv.Connect(ctx, config.AuthKey); err != nil {
		return nil, err
	}

	SetGlobal(srv)
	return srv, nil
}

// Disconnect disconnects from the AceTeam Network.
func Disconnect() error {
	s := Global()
	if s == nil {
		return nil
	}

	err := s.Disconnect()
	ClearGlobal()
	return err
}

// Logout disconnects and clears all state (full logout).
func Logout() error {
	if err := Disconnect(); err != nil {
		return err
	}
	return ClearState()
}

// GetGlobalIPv4 returns the IPv4 address of the global server.
func GetGlobalIPv4() (string, error) {
	s := Global()
	if s == nil {
		return "", fmt.Errorf("not connected to AceTeam Network")
	}
	return s.GetIPv4()
}

// GetGlobalStatus returns the status of the global server.
func GetGlobalStatus(ctx context.Context) (*NetworkStatus, error) {
	s := Global()
	if s == nil {
		return &NetworkStatus{Connected: false}, nil
	}
	return s.Status(ctx)
}

// KeepAlive triggers network activity to keep Headscale's lastSeen fresh.
// Safe to call even if not connected (returns nil).
func KeepAlive(ctx context.Context) error {
	s := Global()
	if s == nil {
		return nil
	}
	return s.KeepAlive(ctx)
}

// GetGlobalNodeID returns the Headscale numeric node ID of the global server.
// Returns empty string if not connected or node ID is unavailable.
func GetGlobalNodeID(ctx context.Context) string {
	s := Global()
	if s == nil {
		return ""
	}
	status, err := s.Status(ctx)
	if err != nil || !status.Connected {
		return ""
	}
	return status.NodeID
}

// GetGlobalPeers returns the list of peers from the global server.
func GetGlobalPeers(ctx context.Context) ([]PeerInfo, error) {
	s := Global()
	if s == nil {
		return nil, fmt.Errorf("not connected to network")
	}
	return s.GetPeers(ctx)
}

// WhoIsPeer resolves a remote address ("ip" or "ip:port") to its verified mesh
// identity via the global server. See NetworkServer.WhoIs and citadel #585.
// Returns an error when not connected or the peer cannot be verified; callers
// must treat that as "unverified" and fall back to token auth.
func WhoIsPeer(ctx context.Context, remoteAddr string) (*PeerIdentity, error) {
	s := Global()
	if s == nil {
		return nil, fmt.Errorf("not connected to AceTeam Network")
	}
	return s.WhoIs(ctx, remoteAddr)
}

// PingPeer pings a peer via the global server.
func PingPeer(ctx context.Context, ip string) (latencyMs float64, connType string, relay string, err error) {
	s := Global()
	if s == nil {
		return 0, "", "", fmt.Errorf("not connected")
	}
	return s.PingPeer(ctx, ip)
}

// Dial connects to a remote address through the global server's network.
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	s := Global()
	if s == nil {
		return nil, fmt.Errorf("not connected to AceTeam Network")
	}
	return s.Dial(ctx, network, addr)
}

// Listen creates a listener on the network for the given address via the global server.
func Listen(network, addr string) (net.Listener, error) {
	s := Global()
	if s == nil {
		return nil, fmt.Errorf("not connected to AceTeam Network")
	}
	return s.Listen(network, addr)
}

// ListenVPN creates a tsnet listener bound to this node's assigned VPN (tailnet)
// IPv4 address on the given port, returning both the listener and the IP it bound
// to so callers can log it.
//
// It must be used instead of Listen(":port") for services that need to be
// reachable at the node's VPN IP (e.g. the terminal, status, and gateway servers
// that the platform relay dials at ws://<vpn_ip>:<port>).
//
// Root cause this addresses (see issue #286): tsnet matches an inbound connection
// to a listener by destination address. A listener opened with an empty host
// (":port") registers under listenKey{addr: <zero>} and is only matched when the
// connection's destination equals one of s.TailscaleIPs() *at connect time*
// (tsnet's listenerForDstAddr conditional fallback). A listener opened with the
// explicit assigned IP registers under listenKey{addr: <ip>} and is matched
// unconditionally on the first lookup. tsnet's own docs instruct callers to
// "explicitly specify the listening address." Binding explicitly makes the VPN
// listener reliably reachable at the node's VPN IP.
//
// Because the tailnet IP may not be assigned the instant the backend reports
// "Running", this waits up to a bounded timeout for GetIPv4() to return a valid
// address before binding.
func ListenVPN(network, port string) (net.Listener, string, error) {
	s := Global()
	if s == nil {
		return nil, "", fmt.Errorf("not connected to AceTeam Network")
	}

	// Wait (bounded) for the tailnet IPv4 to be assigned. A working connection
	// assigns the IP within a second or two; the timeout guards against a node
	// that reported Running but has not yet received its netmap.
	const ipWaitTimeout = 15 * time.Second
	deadline := time.Now().Add(ipWaitTimeout)
	var ip4 string
	for {
		v, err := s.GetIPv4()
		if err == nil && v != "" {
			ip4 = v
			break
		}
		if time.Now().After(deadline) {
			return nil, "", fmt.Errorf("timed out waiting for VPN IPv4 assignment: %w", err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	bindAddr := net.JoinHostPort(ip4, port)
	ln, err := s.Listen(network, bindAddr)
	if err != nil {
		return nil, ip4, fmt.Errorf("listen on %s: %w", bindAddr, err)
	}
	return ln, ip4, nil
}

// VerifyOrReconnect checks connection and reconnects if state exists but not connected.
// Returns (connected, error). No error if simply not logged in.
//
// If state exists but the connection times out (e.g. expired/revoked Headscale key),
// returns ErrStaleState after reconnectAttempts failures. Callers should handle
// this by clearing state and re-authenticating with a fresh authkey.
func VerifyOrReconnect(ctx context.Context) (bool, error) {
	if IsGlobalConnected() {
		return true, nil
	}
	if !HasState() {
		return false, nil
	}

	hostname := getHostnameForReconnect()
	config := ServerConfig{
		Hostname:   hostname,
		ControlURL: DefaultControlURL,
		StateDir:   GetStateDir(),
	}

	// Retry the no-authkey reconnect with backoff. The first attempt may
	// fail simply because the network interface is not ready yet at boot.
	// Retrying here is far cheaper than falling through to ClearState,
	// which destroys the node's WireGuard keys and mints a new Headscale ID.
	for attempt := 1; attempt <= reconnectAttempts; attempt++ {
		if attempt > 1 {
			backoff := time.Duration(attempt) * 5 * time.Second
			if logf != nil {
				logf("reconnect attempt %d/%d in %s...", attempt, reconnectAttempts, backoff)
			}
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(backoff):
			}
		}

		reconnectCtx, cancel := context.WithTimeout(ctx, reconnectTimeout)
		srv := NewServer(config)
		connErr := srv.Connect(reconnectCtx, "")
		cancel()

		if connErr == nil {
			SetGlobal(srv)
			if logf != nil && attempt > 1 {
				logf("reconnected on attempt %d", attempt)
			}
			return true, nil
		}

		// Non-stale errors (e.g. permission denied, state dir missing) won't
		// improve on retry, so bail immediately.
		if !isStaleStateError(connErr) {
			return false, fmt.Errorf("failed to reconnect: %w", connErr)
		}

		if logf != nil {
			logf("reconnect attempt %d/%d failed: %v", attempt, reconnectAttempts, connErr)
		}
	}

	return false, ErrStaleState
}

// ReconnectWithAuthKey attempts to connect using an existing state directory
// and a fresh authkey. This preserves the machine key (and thus the node's
// IP address) while re-authorizing with Headscale.
//
// If this fails, the caller should fall back to ClearState + fresh Connect.
func ReconnectWithAuthKey(ctx context.Context, authKey string) (bool, error) {
	hostname := getHostnameForReconnect()
	config := ServerConfig{
		Hostname:   hostname,
		ControlURL: DefaultControlURL,
		StateDir:   GetStateDir(),
		AuthKey:    authKey,
	}

	srv := NewServer(config)
	reconnectCtx, cancel := context.WithTimeout(ctx, reconnectTimeout)
	defer cancel()

	if err := srv.Connect(reconnectCtx, authKey); err != nil {
		return false, fmt.Errorf("reconnect with fresh authkey failed: %w", err)
	}

	SetGlobal(srv)
	return true, nil
}

// isStaleStateError returns true if the error indicates that the network
// state is stale and cannot be used to reconnect. This happens when:
//   - The connection timed out (context deadline exceeded)
//   - The timeout message from waitForConnection was hit
//   - The authkey/node key was explicitly rejected
func isStaleStateError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "timeout waiting for network connection") ||
		strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "not authorized") ||
		strings.Contains(msg, "key expired") ||
		strings.Contains(msg, "node key rejected")
}

// getHostnameForReconnect reads hostname from manifest or falls back to OS hostname.
func getHostnameForReconnect() string {
	// Try to read from manifest in common locations
	locations := getManifestLocations()
	for _, loc := range locations {
		if hostname := readNodeNameFromManifest(loc); hostname != "" {
			return hostname
		}
	}

	// Fallback to OS hostname
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return "citadel-node"
	}
	return hostname
}

// getManifestLocations returns possible locations for citadel.yaml
func getManifestLocations() []string {
	var locations []string

	// First, try the global config to find node config dir
	globalConfigDir := getGlobalConfigDir()
	globalConfigFile := filepath.Join(globalConfigDir, "config.yaml")
	if nodeDir := readNodeConfigDir(globalConfigFile); nodeDir != "" {
		locations = append(locations, filepath.Join(nodeDir, "citadel.yaml"))
	}

	// Current directory
	if cwd, err := os.Getwd(); err == nil {
		locations = append(locations, filepath.Join(cwd, "citadel.yaml"))
	}

	// Global system config
	locations = append(locations, filepath.Join(globalConfigDir, "citadel.yaml"))

	// User home directory
	if homeDir, err := os.UserHomeDir(); err == nil {
		locations = append(locations, filepath.Join(homeDir, "citadel-node", "citadel.yaml"))
	}

	return locations
}

// getGlobalConfigDir returns the global config directory path
func getGlobalConfigDir() string {
	switch runtime.GOOS {
	case "darwin":
		return "/usr/local/etc/citadel"
	case "windows":
		return filepath.Join(os.Getenv("ProgramData"), "Citadel")
	default:
		return "/etc/citadel"
	}
}

// readNodeConfigDir reads node_config_dir from global config file
func readNodeConfigDir(globalConfigFile string) string {
	data, err := os.ReadFile(globalConfigFile)
	if err != nil {
		return ""
	}

	var config struct {
		NodeConfigDir string `yaml:"node_config_dir"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return ""
	}

	return config.NodeConfigDir
}

// readNodeNameFromManifest reads the node.name field from a citadel.yaml file
func readNodeNameFromManifest(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	var manifest struct {
		Node struct {
			Name string `yaml:"name"`
		} `yaml:"node"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return ""
	}

	return manifest.Node.Name
}
