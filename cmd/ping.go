// cmd/ping.go
package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/spf13/cobra"
)

var pingCount int

var pingCmd = &cobra.Command{
	Use:   "ping [ip-or-hostname]",
	Short: "Ping peers on the AceTeam Network",
	Long: `Pings peer nodes on the AceTeam Network using the internal mesh protocol.

This works even when standard ICMP ping doesn't, because it uses the WireGuard
tunnel's discovery mechanism.

With no arguments, pings all online peers once to show network health.`,
	Example: `  citadel ping                  # Ping all online peers
  citadel ping 100.64.0.25      # Ping specific IP
  citadel ping aceteamvm        # Ping by hostname
  citadel ping 100.64.0.25 -c 5 # Ping 5 times`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		// If no args, ping all peers
		if len(args) == 0 {
			pingAllPeers()
			return
		}

		target := args[0]

		// Check if we're connected
		if !network.HasState() {
			badColor.Println("Not connected to AceTeam Network. Run 'citadel login' first.")
			return
		}

		// Reconnect if needed
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		connected, err := network.VerifyOrReconnect(ctx)
		cancel()
		if !connected {
			if err != nil {
				badColor.Printf("Failed to connect: %v\n", err)
			} else {
				badColor.Println("Not connected to AceTeam Network.")
			}
			return
		}

		// Resolve hostname to IP if needed
		ip := target
		if !isIPAddress(target) {
			resolved, err := resolveHostnameToIP(target)
			if err != nil {
				badColor.Printf("Could not resolve '%s': %v\n", target, err)
				return
			}
			ip = resolved
			fmt.Printf("PING %s (%s)\n", target, ip)
		} else {
			fmt.Printf("PING %s\n", ip)
		}

		// Ping loop
		successCount := 0
		var totalLatency float64

		for i := 0; i < pingCount; i++ {
			pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
			latency, connType, relay, err := network.PingPeer(pingCtx, ip)
			pingCancel()

			if err != nil {
				badColor.Printf("  Request timeout: %v\n", err)
			} else {
				successCount++
				totalLatency += latency

				connInfo := ""
				if connType == "relay" && relay != "" {
					connInfo = fmt.Sprintf(" via relay:%s", relay)
				} else if connType == "direct" {
					connInfo = " direct"
				}

				goodColor.Printf("  Reply from %s: %.1fms%s\n", ip, latency, connInfo)
			}

			// Sleep between pings (except for last one)
			if i < pingCount-1 {
				time.Sleep(1 * time.Second)
			}
		}

		// Summary
		fmt.Println()
		lossPercent := float64(pingCount-successCount) / float64(pingCount) * 100
		fmt.Printf("--- %s ping statistics ---\n", target)
		fmt.Printf("%d packets transmitted, %d received, %.0f%% packet loss\n",
			pingCount, successCount, lossPercent)
		if successCount > 0 {
			avgLatency := totalLatency / float64(successCount)
			fmt.Printf("avg latency: %.1fms\n", avgLatency)
		}
	},
}

func pingAllPeers() {
	// Check if we're connected
	if !network.HasState() {
		badColor.Println("Not connected to AceTeam Network. Run 'citadel login' first.")
		return
	}

	// Reconnect if needed
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	connected, err := network.VerifyOrReconnect(ctx)
	cancel()
	if !connected {
		if err != nil {
			badColor.Printf("Failed to connect: %v\n", err)
		} else {
			badColor.Println("Not connected to AceTeam Network.")
		}
		return
	}

	// Get our own IP to filter ourselves out
	myIP, _ := network.GetGlobalIPv4()

	// Get all peers
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	peers, err := network.GetGlobalPeers(ctx)
	cancel()
	if err != nil {
		badColor.Printf("Failed to get peers: %v\n", err)
		return
	}

	// Filter to other peers only
	var otherPeers []network.PeerInfo
	for _, peer := range peers {
		if peer.IP != myIP && peer.IP != "" {
			otherPeers = append(otherPeers, peer)
		}
	}

	if len(otherPeers) == 0 {
		fmt.Println("No other peers on network.")
		return
	}

	fmt.Printf("Pinging %d peers...\n\n", len(otherPeers))

	reachable := 0
	unreachable := 0

	for _, peer := range otherPeers {
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
		latency, connType, relay, err := network.PingPeer(pingCtx, peer.IP)
		pingCancel()

		if err != nil {
			unreachable++
			badColor.Printf("  ⚫ %s %s - unreachable\n", peer.Hostname, peer.IP)
		} else {
			reachable++
			connInfo := ""
			if connType == "relay" && relay != "" {
				connInfo = fmt.Sprintf(" [relay:%s]", relay)
			} else if connType == "direct" {
				connInfo = " [direct]"
			}

			osInfo := ""
			if peer.OS != "" {
				osInfo = fmt.Sprintf(" (%s)", peer.OS)
			}

			goodColor.Printf("  🟢 %s %s %.0fms%s%s\n", peer.Hostname, peer.IP, latency, connInfo, osInfo)
		}
	}

	fmt.Printf("\n%d reachable, %d unreachable\n", reachable, unreachable)
}

func isIPAddress(s string) bool {
	// Simple check: if it contains dots and all parts are numbers, it's likely an IP
	for _, c := range s {
		if c != '.' && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func resolveHostnameToIP(hostname string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	peers, err := network.GetGlobalPeers(ctx)
	if err != nil {
		return "", err
	}

	for _, peer := range peers {
		if peer.Hostname == hostname && peer.IP != "" {
			return peer.IP, nil
		}
	}

	return "", fmt.Errorf("hostname not found in network peers")
}

func init() {
	rootCmd.AddCommand(pingCmd)
	pingCmd.Flags().IntVarP(&pingCount, "count", "c", 3, "Number of pings to send")
}
