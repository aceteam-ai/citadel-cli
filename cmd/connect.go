// cmd/connect.go
package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
	"github.com/spf13/cobra"
)

var (
	connectTimeout int
)

var connectCmd = &cobra.Command{
	Use:   "connect [peer:port]",
	Short: "Connect to a service on another node",
	Long: `Establishes a raw TCP connection to a service on another node and pipes
stdin/stdout to it.

This is useful for:
  - Testing connectivity to services
  - Piping data through the network
  - Using as SSH ProxyCommand (used internally by 'citadel ssh')

PEER IDENTIFICATION:
  You can specify the peer in multiple ways:
  - By hostname:  citadel connect gpu-node-1:5432
  - By IP:        citadel connect 100.64.0.25:5432
  - Interactive:  citadel connect  (prompts for peer and port)

The connection uses the tsnet userspace network, so it can reach peers
that system networking cannot.`,
	Example: `  # Interactive mode - select peer and port
  citadel connect

  # Connect to a PostgreSQL server
  citadel connect gpu-node-1:5432

  # Connect to Redis
  citadel connect gpu-node-1:6379

  # Connect by IP address
  citadel connect 100.64.0.25:11434

  # Connect with timeout
  citadel connect gpu-node-1:11434 --timeout 30`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		// Ensure network connection
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := ensureNetworkConnected(ctx); err != nil {
			cancel()
			badColor.Println(err)
			os.Exit(1)
		}
		cancel()

		var peer, port string
		var err error

		// Interactive mode if no args
		if len(args) == 0 {
			peer, port, err = setupConnectInteractive()
			if err != nil {
				badColor.Printf("Error: %v\n", err)
				os.Exit(1)
			}
		} else {
			// Parse peer:port
			peer, port, err = parsePeerPort(args[0])
			if err != nil {
				badColor.Printf("Invalid target: %v\n", err)
				fmt.Println("Usage: citadel connect <peer>:<port>")
				os.Exit(1)
			}
		}

		// Resolve peer to IP
		ip, hostname, err := resolvePeer(peer)
		if err != nil {
			badColor.Printf("Could not resolve peer '%s': %v\n", peer, err)
			suggestAvailablePeers()
			os.Exit(1)
		}

		// Display connection info
		addr := fmt.Sprintf("%s:%s", ip, port)
		if hostname != "" {
			fmt.Fprintf(os.Stderr, "Connecting to %s (%s)...\n", hostname, addr)
		} else {
			fmt.Fprintf(os.Stderr, "Connecting to %s...\n", addr)
		}

		// Set up connection context with timeout
		connectCtx := context.Background()
		var connectCancel context.CancelFunc
		if connectTimeout > 0 {
			connectCtx, connectCancel = context.WithTimeout(connectCtx, time.Duration(connectTimeout)*time.Second)
			defer connectCancel()
		}

		// Dial through the network
		conn, err := network.Dial(connectCtx, "tcp", addr)
		if err != nil {
			badColor.Printf("Connection failed: %v\n", err)
			os.Exit(1)
		}
		defer conn.Close()

		fmt.Fprintf(os.Stderr, "Connected.\n")

		// Handle interrupt
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigChan
			conn.Close()
		}()

		// Bidirectional copy
		done := make(chan struct{})

		// Copy from connection to stdout
		go func() {
			io.Copy(os.Stdout, conn)
			done <- struct{}{}
		}()

		// Copy from stdin to connection
		go func() {
			io.Copy(conn, os.Stdin)
			// When stdin is closed, close the write side of the connection
			if tcpConn, ok := conn.(interface{ CloseWrite() error }); ok {
				tcpConn.CloseWrite()
			}
			done <- struct{}{}
		}()

		// Wait for either direction to complete
		<-done
	},
}

// setupConnectInteractive guides the user through setting up a connection interactively.
func setupConnectInteractive() (peer string, port string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Get our own IP to filter ourselves out
	myIP, _ := network.GetGlobalIPv4()

	// Get peers
	peers, err := network.GetGlobalPeers(ctx)
	if err != nil {
		return "", "", fmt.Errorf("failed to get peers: %w", err)
	}

	// Filter to online peers (excluding ourselves)
	var choices []string
	var peerMap = make(map[string]string) // display -> hostname

	for _, p := range peers {
		if p.IP != "" && p.IP != myIP && p.Online {
			display := fmt.Sprintf("%s (%s)", p.Hostname, p.IP)
			if p.OS != "" {
				display = fmt.Sprintf("%s (%s) [%s]", p.Hostname, p.IP, p.OS)
			}
			choices = append(choices, display)
			peerMap[display] = p.Hostname
		}
	}

	if len(choices) == 0 {
		return "", "", fmt.Errorf("no online peers found on the network")
	}

	// Select peer
	fmt.Println("Select a peer to connect to:")
	fmt.Println()
	selected, err := ui.AskSelect("Available peers:", choices)
	if err != nil {
		return "", "", err
	}
	peer = peerMap[selected]

	// Select service/port
	fmt.Println()
	serviceChoices := []string{
		"SSH (22)",
		"Ollama (11434)",
		"vLLM (8000)",
		"llama.cpp (8080)",
		"PostgreSQL (5432)",
		"Redis (6379)",
		"Custom port...",
	}
	serviceSelected, err := ui.AskSelect("Which service/port?", serviceChoices)
	if err != nil {
		return "", "", err
	}

	// Map service to port
	servicePorts := map[string]string{
		"SSH (22)":          "22",
		"Ollama (11434)":    "11434",
		"vLLM (8000)":       "8000",
		"llama.cpp (8080)":  "8080",
		"PostgreSQL (5432)": "5432",
		"Redis (6379)":      "6379",
	}

	if p, ok := servicePorts[serviceSelected]; ok {
		port = p
	} else {
		// Custom port
		fmt.Println()
		port, err = ui.AskInput("Enter port:", "8080", "")
		if err != nil {
			return "", "", err
		}
	}

	return peer, port, nil
}

func init() {
	rootCmd.AddCommand(connectCmd)
	connectCmd.Flags().IntVar(&connectTimeout, "timeout", 0, "Connection timeout in seconds (0 = no timeout)")
}
