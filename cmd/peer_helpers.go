// cmd/peer_helpers.go
package cmd

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
)

// resolvePeer resolves a peer identifier (hostname or IP) to its network IP.
// Returns the resolved IP, the hostname (if resolved from hostname), and any error.
func resolvePeer(peer string) (ip string, hostname string, err error) {
	// Check if it's already an IP address
	if isValidIP(peer) {
		return peer, "", nil
	}

	// Resolve hostname to IP from network peers
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	peers, err := network.GetGlobalPeers(ctx)
	if err != nil {
		return "", "", fmt.Errorf("failed to get peers: %w", err)
	}

	// Try exact match first
	for _, p := range peers {
		if p.Hostname == peer && p.IP != "" {
			return p.IP, p.Hostname, nil
		}
	}

	// Try case-insensitive match
	peerLower := strings.ToLower(peer)
	for _, p := range peers {
		if strings.ToLower(p.Hostname) == peerLower && p.IP != "" {
			return p.IP, p.Hostname, nil
		}
	}

	// Try partial match (prefix match)
	for _, p := range peers {
		if strings.HasPrefix(strings.ToLower(p.Hostname), peerLower) && p.IP != "" {
			return p.IP, p.Hostname, nil
		}
	}

	return "", "", fmt.Errorf("peer '%s' not found on network", peer)
}

// isValidIP checks if the string is a valid IP address.
func isValidIP(s string) bool {
	_, err := netip.ParseAddr(s)
	return err == nil
}

// ensureNetworkConnected verifies the network connection and reconnects if needed.
// Returns an error if the network cannot be established.
func ensureNetworkConnected(ctx context.Context) error {
	if !network.HasState() {
		return fmt.Errorf("not connected to AceTeam Network - run 'citadel login' first")
	}

	connected, err := network.VerifyOrReconnect(ctx)
	if !connected {
		if err != nil {
			return fmt.Errorf("failed to connect to network: %w", err)
		}
		return fmt.Errorf("not connected to AceTeam Network")
	}

	return nil
}

// suggestAvailablePeers prints a list of available peers that can be connected to.
func suggestAvailablePeers() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	peers, err := network.GetGlobalPeers(ctx)
	if err != nil {
		return
	}

	// Filter to online peers
	var onlinePeers []network.PeerInfo
	myIP, _ := network.GetGlobalIPv4()
	for _, p := range peers {
		if p.Online && p.IP != "" && p.IP != myIP {
			onlinePeers = append(onlinePeers, p)
		}
	}

	if len(onlinePeers) == 0 {
		fmt.Println("\nNo other peers are currently online.")
		return
	}

	fmt.Println("\nAvailable peers:")
	for _, p := range onlinePeers {
		fmt.Printf("  - %s (%s)\n", p.Hostname, p.IP)
	}
}

// parsePeerPort parses a "peer:port" string and returns the peer and port.
func parsePeerPort(s string) (peer string, port string, err error) {
	// Handle IPv6 addresses in brackets: [::1]:8080
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end == -1 {
			return "", "", fmt.Errorf("invalid address format: unclosed bracket")
		}
		peer = s[1:end]
		rest := s[end+1:]
		if !strings.HasPrefix(rest, ":") {
			return "", "", fmt.Errorf("invalid address format: missing port")
		}
		port = rest[1:]
		return peer, port, nil
	}

	// Simple hostname:port or ip:port
	lastColon := strings.LastIndex(s, ":")
	if lastColon == -1 {
		return "", "", fmt.Errorf("invalid address format: missing port (use peer:port)")
	}

	peer = s[:lastColon]
	port = s[lastColon+1:]

	if peer == "" {
		return "", "", fmt.Errorf("invalid address format: missing peer")
	}
	if port == "" {
		return "", "", fmt.Errorf("invalid address format: missing port")
	}

	return peer, port, nil
}
