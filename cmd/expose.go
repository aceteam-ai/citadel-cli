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
	Short: "Expose a local port over the AceTeam network",
	Long: `Expose a local service port so other nodes in the network can access it.

Services are accessible via your network IP address. Use 'citadel expose' without
arguments to show access information for your node.

Note: Port exposure via 'citadel expose <port>' requires the worker daemon to be
running. For simple access, services are automatically accessible via your network
IP when connected.`,
	Example: `  # Show current access info (network IP and common service URLs)
  citadel expose

  # Show known service ports with access URLs
  citadel expose --service ollama`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		// Handle --list flag
		if exposeList {
			listExposedPorts()
			return
		}

		// Handle --remove flag
		if exposeRemove {
			if len(args) == 0 && exposeService == "" {
				fmt.Fprintln(os.Stderr, "Error: specify a port or --service to remove")
				os.Exit(1)
			}
			port := getPort(args)
			removeExposedPort(port)
			return
		}

		// If no args and no service, show access info
		if len(args) == 0 && exposeService == "" {
			showAccessInfo()
			return
		}

		// Expose a port
		port := getPort(args)
		exposePort(port)
	},
}

func getPort(args []string) int {
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

func init() {
	rootCmd.AddCommand(exposeCmd)
	exposeCmd.Flags().StringVar(&exposeService, "service", "", "Show access info for a known service (vllm, ollama, llamacpp, lmstudio)")
	exposeCmd.Flags().BoolVar(&exposeList, "list", false, "List currently exposed ports")
	exposeCmd.Flags().BoolVar(&exposeRemove, "remove", false, "Stop exposing a port")
}
