// internal/network/singleton.go
// Global NetworkServer instance management
package network

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"gopkg.in/yaml.v3"
)

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

// GetGlobalPeers returns the list of peers from the global server.
func GetGlobalPeers(ctx context.Context) ([]PeerInfo, error) {
	s := Global()
	if s == nil {
		return nil, fmt.Errorf("not connected to network")
	}
	return s.GetPeers(ctx)
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

// VerifyOrReconnect checks connection and reconnects if state exists but not connected.
// Returns (connected, error). No error if simply not logged in.
func VerifyOrReconnect(ctx context.Context) (bool, error) {
	if IsGlobalConnected() {
		return true, nil
	}
	if !HasState() {
		return false, nil
	}

	// Reconnect using saved state
	hostname := getHostnameForReconnect()
	config := ServerConfig{
		Hostname:   hostname,
		ControlURL: DefaultControlURL,
		StateDir:   GetStateDir(),
	}

	srv := NewServer(config)
	if err := srv.Connect(ctx, ""); err != nil {
		return false, fmt.Errorf("failed to reconnect: %w", err)
	}

	SetGlobal(srv)
	return true, nil
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
