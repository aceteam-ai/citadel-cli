// cmd/proxy.go
package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
	"github.com/spf13/cobra"
)

var (
	proxyBind     string
	proxyVerbose  bool
	proxyMaxConns int
)

var proxyCmd = &cobra.Command{
	Use:   "proxy [local-port] [peer:port]",
	Short: "Forward a local port to a service on another node",
	Long: `Creates a local TCP proxy that forwards connections to a service on another node.

This allows you to access services on remote peers using localhost. Any connection
to the local port will be forwarded through the AceTeam Network to the remote peer.

PEER IDENTIFICATION:
  You can specify the peer in multiple ways:
  - By hostname:  citadel proxy 8080 gpu-node-1:11434
  - By IP:        citadel proxy 8080 100.64.0.25:11434
  - Interactive:  citadel proxy  (prompts for peer and ports)

COMMON SERVICES:
  - Ollama:    port 11434
  - vLLM:      port 8000
  - llama.cpp: port 8080
  - PostgreSQL: port 5432
  - Redis:     port 6379

The proxy runs until interrupted (Ctrl+C).`,
	Example: `  # Interactive mode - guided setup
  citadel proxy

  # Forward localhost:8080 to Ollama on gpu-node-1
  citadel proxy 8080 gpu-node-1:11434
  # Then access via: curl http://localhost:8080/v1/models

  # Forward localhost:5432 to PostgreSQL
  citadel proxy 5432 db-server:5432

  # Forward with verbose logging
  citadel proxy 8080 gpu-node-1:11434 --verbose

  # Bind to all interfaces (not just localhost)
  citadel proxy 8080 gpu-node-1:11434 --bind 0.0.0.0`,
	Args: cobra.MaximumNArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		// Ensure network connection
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := ensureNetworkConnected(ctx); err != nil {
			cancel()
			badColor.Println(err)
			os.Exit(1)
		}
		cancel()

		var localPort int
		var peer, remotePort string
		var err error

		// Interactive mode if not enough args
		if len(args) < 2 {
			localPort, peer, remotePort, err = setupProxyInteractive()
			if err != nil {
				badColor.Printf("Error: %v\n", err)
				os.Exit(1)
			}
		} else {
			// Parse arguments
			localPort, err = strconv.Atoi(args[0])
			if err != nil || localPort < 1 || localPort > 65535 {
				badColor.Printf("Invalid local port: %s\n", args[0])
				os.Exit(1)
			}

			peer, remotePort, err = parsePeerPort(args[1])
			if err != nil {
				badColor.Printf("Invalid target: %v\n", err)
				fmt.Println("Usage: citadel proxy <local-port> <peer>:<port>")
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

		remoteAddr := fmt.Sprintf("%s:%s", ip, remotePort)
		localAddr := fmt.Sprintf("%s:%d", proxyBind, localPort)

		// Start local listener
		listener, err := net.Listen("tcp", localAddr)
		if err != nil {
			badColor.Printf("Failed to listen on %s: %v\n", localAddr, err)
			os.Exit(1)
		}
		defer listener.Close()

		// Display info
		fmt.Println()
		goodColor.Println("Proxy started!")
		fmt.Printf("  Local:  %s\n", localAddr)
		if hostname != "" {
			fmt.Printf("  Remote: %s (%s)\n", hostname, remoteAddr)
		} else {
			fmt.Printf("  Remote: %s\n", remoteAddr)
		}
		fmt.Println()
		fmt.Println("Press Ctrl+C to stop.")
		fmt.Println()

		// Track active connections
		var activeConns int64
		var wg sync.WaitGroup

		// Handle interrupt
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigChan
			fmt.Println("\nShutting down...")
			listener.Close()
		}()

		// Accept connections
		for {
			conn, err := listener.Accept()
			if err != nil {
				// Check if listener was closed
				select {
				case <-sigChan:
					// Normal shutdown
				default:
					if proxyVerbose {
						badColor.Printf("Accept error: %v\n", err)
					}
				}
				break
			}

			// Check max connections limit
			if proxyMaxConns > 0 && atomic.LoadInt64(&activeConns) >= int64(proxyMaxConns) {
				if proxyVerbose {
					warnColor.Printf("Max connections (%d) reached, rejecting %s\n", proxyMaxConns, conn.RemoteAddr())
				}
				conn.Close()
				continue
			}

			atomic.AddInt64(&activeConns, 1)
			wg.Add(1)

			go func(localConn net.Conn) {
				defer func() {
					localConn.Close()
					atomic.AddInt64(&activeConns, -1)
					wg.Done()
				}()

				if proxyVerbose {
					fmt.Printf("New connection from %s\n", localConn.RemoteAddr())
				}

				// Connect to remote
				dialCtx, dialCancel := context.WithTimeout(context.Background(), 30*time.Second)
				remoteConn, err := network.Dial(dialCtx, "tcp", remoteAddr)
				dialCancel()
				if err != nil {
					if proxyVerbose {
						badColor.Printf("Failed to connect to %s: %v\n", remoteAddr, err)
					}
					return
				}
				defer remoteConn.Close()

				if proxyVerbose {
					goodColor.Printf("Connected to %s\n", remoteAddr)
				}

				// Bidirectional copy
				done := make(chan struct{}, 2)

				go func() {
					io.Copy(remoteConn, localConn)
					if tcpConn, ok := remoteConn.(interface{ CloseWrite() error }); ok {
						tcpConn.CloseWrite()
					}
					done <- struct{}{}
				}()

				go func() {
					io.Copy(localConn, remoteConn)
					if tcpConn, ok := localConn.(interface{ CloseWrite() error }); ok {
						tcpConn.CloseWrite()
					}
					done <- struct{}{}
				}()

				// Wait for both directions to complete
				<-done
				<-done

				if proxyVerbose {
					fmt.Printf("Connection from %s closed\n", localConn.RemoteAddr())
				}
			}(conn)
		}

		// Wait for all connections to finish
		wg.Wait()
		fmt.Println("Proxy stopped.")
	},
}

// setupProxyInteractive guides the user through setting up a proxy interactively.
func setupProxyInteractive() (localPort int, peer string, remotePort string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Get our own IP to filter ourselves out
	myIP, _ := network.GetGlobalIPv4()

	// Get peers
	peers, err := network.GetGlobalPeers(ctx)
	if err != nil {
		return 0, "", "", fmt.Errorf("failed to get peers: %w", err)
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
		return 0, "", "", fmt.Errorf("no online peers found on the network")
	}

	// Select peer
	fmt.Println("Select a peer to forward to:")
	fmt.Println()
	selected, err := ui.AskSelect("Available peers:", choices)
	if err != nil {
		return 0, "", "", err
	}
	peer = peerMap[selected]

	// Select service/port
	fmt.Println()
	serviceChoices := []string{
		"Ollama (11434)",
		"vLLM (8000)",
		"llama.cpp (8080)",
		"PostgreSQL (5432)",
		"Redis (6379)",
		"Custom port...",
	}
	serviceSelected, err := ui.AskSelect("Which service?", serviceChoices)
	if err != nil {
		return 0, "", "", err
	}

	// Map service to port
	servicePorts := map[string]string{
		"Ollama (11434)":    "11434",
		"vLLM (8000)":       "8000",
		"llama.cpp (8080)":  "8080",
		"PostgreSQL (5432)": "5432",
		"Redis (6379)":      "6379",
	}

	if port, ok := servicePorts[serviceSelected]; ok {
		remotePort = port
	} else {
		// Custom port
		fmt.Println()
		portStr, err := ui.AskInput("Enter remote port:", "8080", "")
		if err != nil {
			return 0, "", "", err
		}
		remotePort = portStr
	}

	// Ask for local port (default to same as remote)
	fmt.Println()
	localPortStr, err := ui.AskInput("Enter local port:", remotePort, remotePort)
	if err != nil {
		return 0, "", "", err
	}

	localPort, err = strconv.Atoi(localPortStr)
	if err != nil || localPort < 1 || localPort > 65535 {
		return 0, "", "", fmt.Errorf("invalid local port: %s", localPortStr)
	}

	return localPort, peer, remotePort, nil
}

func init() {
	rootCmd.AddCommand(proxyCmd)
	proxyCmd.Flags().StringVar(&proxyBind, "bind", "127.0.0.1", "Address to bind to (default localhost only)")
	proxyCmd.Flags().BoolVarP(&proxyVerbose, "verbose", "v", false, "Show connection activity")
	proxyCmd.Flags().IntVar(&proxyMaxConns, "max-conns", 0, "Maximum concurrent connections (0 = unlimited)")
}
