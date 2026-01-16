// internal/network/singleton.go
// Global NetworkServer instance management
package network

import (
	"context"
	"fmt"
	"sync"
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
