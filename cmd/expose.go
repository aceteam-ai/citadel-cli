// cmd/expose.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/spf13/cobra"
)

var (
	exposeService string
	exposeList    bool
	exposeRemove  bool
	exposePeers   bool
	exposeCheck   bool
)

// Known service ports
var servicePorts = map[string]int{
	"vllm":     8000,
	"ollama":   11434,
	"llamacpp": 8080,
	"lmstudio": 1234,
}

var exposeCmd = &cobra.Command{
	Use:   "expose [port]",
	Short: "Show access information for this node and network peers",
	Long: `Shows network access information for this node and services on the AceTeam Network.

Without arguments, displays this node's network IP and common service URLs.
Use --peers to see services available on other nodes in your network.
Use --check to verify that services are actually reachable.`,
	Example: `  # Show this node's access info (network IP and common service URLs)
  citadel expose

  # Show services available on all peers
  citadel expose --peers

  # Show this node's services and verify they're reachable
  citadel expose --check

  # Show peer services with reachability check
  citadel expose --peers --check`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		// Handle --list flag (deprecated, same as no args)
		if exposeList {
			listExposedPorts()
			return
		}

		// Handle --remove flag (legacy)
		if exposeRemove {
			if len(args) == 0 && exposeService == "" {
				fmt.Fprintln(os.Stderr, "Error: specify a port or --service to remove")
				os.Exit(1)
			}
			port := getExposePort(args)
			removeExposedPort(port)
			return
		}

		// Handle --peers flag
		if exposePeers {
			showPeerServices()
			return
		}

		// Handle --check flag (with or without --peers)
		if exposeCheck {
			showAccessInfoWithCheck()
			return
		}

		// If no args and no service, show access info
		if len(args) == 0 && exposeService == "" {
			showAccessInfo()
			return
		}

		// Expose a port (legacy behavior)
		port := getExposePort(args)
		exposePort(port)
	},
}

func getExposePort(args []string) int {
	// If service flag is set, use service port
	if exposeService != "" {
		port, ok := servicePorts[exposeService]
		if !ok {
			fmt.Fprintf(os.Stderr, "Error: unknown service '%s'\n", exposeService)
			fmt.Fprintln(os.Stderr, "Known services: vllm, ollama, llamacpp, lmstudio")
			os.Exit(1)
		}
		return port
	}

	// Otherwise parse port from args
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Error: specify a port number")
		os.Exit(1)
	}

	port, err := strconv.Atoi(args[0])
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintf(os.Stderr, "Error: invalid port number '%s'\n", args[0])
		os.Exit(1)
	}
	return port
}

func showAccessInfo() {
	fmt.Println("Network Access Information")
	fmt.Println("==========================")
	fmt.Println()

	// Check if connected
	if !network.HasState() {
		fmt.Fprintln(os.Stderr, "Error: not connected to AceTeam Network")
		fmt.Fprintln(os.Stderr, "Run 'citadel login' to connect first")
		os.Exit(1)
	}

	// Get network status
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := network.GetGlobalStatus(ctx)
	if err != nil || !status.Connected {
		fmt.Fprintln(os.Stderr, "Error: not connected to AceTeam Network")
		fmt.Fprintln(os.Stderr, "Run 'citadel login' to connect first")
		os.Exit(1)
	}

	// Display access info
	if status.IPv4 != "" {
		fmt.Printf("Network IP:  %s\n", status.IPv4)
	}
	if status.Hostname != "" {
		fmt.Printf("Hostname:    %s\n", status.Hostname)
	}
	fmt.Println()

	// Show known service ports
	fmt.Println("Common Service Ports:")
	for name, port := range servicePorts {
		if status.IPv4 != "" {
			fmt.Printf("  %s: http://%s:%d\n", name, status.IPv4, port)
		}
	}
	fmt.Println()
	fmt.Println("Services are accessible via your Network IP when running.")
}

func exposePort(port int) {
	fmt.Printf("Port exposure via this command is not yet implemented.\n\n")
	fmt.Println("Your services are accessible via your network IP when connected.")
	fmt.Println("Run 'citadel expose' (without arguments) to see your access URLs.")
}

func removeExposedPort(port int) {
	fmt.Printf("Port %d exposure removal is not yet implemented.\n", port)
	fmt.Println("Services are directly accessible via network IP - no explicit exposure needed.")
}

func listExposedPorts() {
	fmt.Println("Port exposure listing is not yet implemented.")
	fmt.Println("Services are directly accessible via network IP when running.")
	fmt.Println("Run 'citadel expose' to see your access URLs.")
}

// showPeerServices displays services available on all network peers.
func showPeerServices() {
	fmt.Println("Peer Services on AceTeam Network")
	fmt.Println("=================================")
	fmt.Println()

	// Ensure network connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ensureNetworkConnected(ctx); err != nil {
		badColor.Println(err)
		os.Exit(1)
	}

	// Get our own IP to filter ourselves out
	myIP, _ := network.GetGlobalIPv4()

	// Get peers
	peers, err := network.GetGlobalPeers(ctx)
	if err != nil {
		badColor.Printf("Failed to get peers: %v\n", err)
		os.Exit(1)
	}

	// Filter to other online peers
	var onlinePeers []network.PeerInfo
	for _, peer := range peers {
		if peer.IP != "" && peer.IP != myIP {
			onlinePeers = append(onlinePeers, peer)
		}
	}

	if len(onlinePeers) == 0 {
		fmt.Println("No other peers found on the network.")
		return
	}

	for _, peer := range onlinePeers {
		statusIcon := "⚫"
		if peer.Online {
			statusIcon = goodColor.Sprint("🟢")
		}

		fmt.Printf("%s %s (%s)\n", statusIcon, peer.Hostname, peer.IP)

		if peer.Online {
			// Show potential service URLs
			for name, port := range servicePorts {
				fmt.Printf("     %s: http://%s:%d\n", name, peer.IP, port)
			}

			// Show proxy hint
			fmt.Printf("     Use: citadel proxy <local-port> %s:<port>\n", peer.Hostname)
		} else {
			fmt.Println("     (offline)")
		}
		fmt.Println()
	}

	fmt.Println("Tip: Use 'citadel expose --peers --check' to verify service reachability.")
}

// showAccessInfoWithCheck shows access info and verifies service connectivity.
func showAccessInfoWithCheck() {
	fmt.Println("Service Reachability Check")
	fmt.Println("==========================")
	fmt.Println()

	// Ensure network connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ensureNetworkConnected(ctx); err != nil {
		badColor.Println(err)
		os.Exit(1)
	}

	// Get network status
	status, err := network.GetGlobalStatus(ctx)
	if err != nil || !status.Connected {
		badColor.Println("Not connected to AceTeam Network")
		os.Exit(1)
	}

	// Display our info
	fmt.Printf("This Node: %s (%s)\n\n", status.Hostname, status.IPv4)

	// If --peers is also set, check peer services
	if exposePeers {
		checkPeerServices()
	} else {
		// Check local services
		fmt.Println("Checking local service ports...")
		for name, port := range servicePorts {
			checkServicePort(status.IPv4, port, name)
		}
	}
}

// checkPeerServices checks connectivity to services on all peers.
func checkPeerServices() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	myIP, _ := network.GetGlobalIPv4()
	peers, err := network.GetGlobalPeers(ctx)
	if err != nil {
		badColor.Printf("Failed to get peers: %v\n", err)
		return
	}

	for _, peer := range peers {
		if peer.IP == "" || peer.IP == myIP || !peer.Online {
			continue
		}

		fmt.Printf("\n%s (%s):\n", peer.Hostname, peer.IP)
		for name, port := range servicePorts {
			checkServicePort(peer.IP, port, name)
		}
	}
}

// checkServicePort attempts to connect to a port and reports success/failure.
func checkServicePort(ip string, port int, serviceName string) {
	addr := fmt.Sprintf("%s:%d", ip, port)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := network.Dial(ctx, "tcp", addr)
	if err != nil {
		badColor.Printf("  %s (:%d): ❌ unreachable\n", serviceName, port)
		return
	}
	conn.Close()
	goodColor.Printf("  %s (:%d): ✅ reachable\n", serviceName, port)
}

func init() {
	rootCmd.AddCommand(exposeCmd)
	exposeCmd.Flags().StringVar(&exposeService, "service", "", "Show access info for a known service (vllm, ollama, llamacpp, lmstudio)")
	exposeCmd.Flags().BoolVar(&exposePeers, "peers", false, "Show services available on all network peers")
	exposeCmd.Flags().BoolVar(&exposeCheck, "check", false, "Verify service reachability (can combine with --peers)")
	exposeCmd.Flags().BoolVar(&exposeList, "list", false, "List currently exposed ports (deprecated)")
	exposeCmd.Flags().BoolVar(&exposeRemove, "remove", false, "Stop exposing a port (deprecated)")
	exposeCmd.Flags().MarkHidden("list")
	exposeCmd.Flags().MarkHidden("remove")
}
