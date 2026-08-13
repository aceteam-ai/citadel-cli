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
	connectTimeout      int
	connectTerminalPort int
	connectToken        string
)

var connectCmd = &cobra.Command{
	Use:   "connect [peer|peer:port]",
	Short: "Open a shell on another node, or pipe a raw TCP service",
	Long: `Two modes, selected by whether you give a port:

  citadel connect <node-name | ip>          Open a shell on the target
  citadel connect <node-name | ip>:<port>   Pipe a raw TCP service (stdin/stdout)

BARE TARGET (no port):
  This is an alias of 'citadel ssh <peer>' (see 'citadel ssh --help'): both
  commands route a bare target through the exact same logic (issue #754).

  By default this tries the AceTeam Network terminal endpoint first,
  collapsing the old multi-hop SSH chain (ssh a -> ssh b -> tmux) into a
  single command from any machine on the mesh, no host SSH config, no
  '.local' mDNS, no manual hops, and it works even on nodes whose host sshd
  is not exposed on the mesh. A node passcode challenge is NOT treated as a
  failure to fall back on: you are prompted for it interactively instead.
  Only when the terminal endpoint itself is unreachable does this fall back
  to a real OpenSSH connection (tunneled through the mesh to the peer's
  sshd, port 22 by default), the pre-#754 behavior of 'citadel ssh'.

  The terminal endpoint is started by 'citadel work' by default (disable
  with --no-terminal). No token is needed there: the node trusts your
  verified mesh-peer identity over the VPN (citadel #585). Repeated connects
  re-attach to the same live tmux-backed shell. A --token (or
  CITADEL_TERMINAL_TOKEN) is still accepted for the platform terminal path or
  when the target disables mesh trust.

  --raw (alias --via-sshd) skips straight to the OpenSSH path.
  --mesh (alias --terminal) forces the terminal-endpoint path only, with no
  OpenSSH fallback.
  A node passcode (when the operator has set one) is supplied via --passcode
  or CITADEL_TERMINAL_PASSCODE, or prompted for interactively. -u/--user and
  -p/--port for the OpenSSH path are only on 'citadel ssh'.

RAW TCP (with port):
  Establishes a raw TCP connection to a service on the target and pipes
  stdin/stdout to it. Used internally by the OpenSSH fallback as a
  ProxyCommand, and useful for testing connectivity or piping data through
  the network. Unaffected by any of the above: always a direct dial, no
  terminal-endpoint attempt, no passcode prompt.

PEER IDENTIFICATION (both modes):
  - By hostname:  citadel connect gpu-node-1
  - By IP:        citadel connect 100.64.0.25
  - Interactive:  citadel connect  (prompts for peer and port, raw TCP)

The connection uses the tsnet userspace network, so it can reach peers
that system networking cannot.`,
	Example: `  # Open a shell (by name or mesh IP; tries the terminal endpoint, falls back to sshd)
  citadel connect gpu-node-1
  citadel connect 100.64.0.25

  # Shell with an explicit terminal token / port
  citadel connect gpu-node-1 --token tok_... --terminal-port 7860

  # Force the terminal-endpoint path only, no OpenSSH fallback
  citadel connect gpu-node-1 --mesh

  # Raw TCP: connect to a PostgreSQL server
  citadel connect gpu-node-1:5432

  # Raw TCP by IP address
  citadel connect 100.64.0.25:11434

  # Interactive mode - select peer and port (raw TCP)
  citadel connect`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		// Bare-target mode: a single target (name or IP) with no :port,
		// shared with 'citadel ssh' via connectToNode (#754). A port
		// (host:port) always routes to the existing raw-TCP path below,
		// which keeps the OpenSSH fallback's ProxyCommand (always ip:port)
		// unchanged.
		if len(args) == 1 && connectIsShellTarget(args[0]) {
			if err := connectToNode(cmd, args[0]); err != nil {
				badColor.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			return
		}

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
		"WeChat (8000)",
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
		"WeChat (8000)":     "8000",
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

// connectIsShellTarget reports whether a single positional argument should be
// treated as a remote-shell target (a bare node name or mesh/LAN IP) rather
// than a raw-TCP "host:port". The ordering matters because tsnet also assigns
// IPv6 addresses (all-colons), which must not be misread as host:port:
//
//  1. A valid IP (v4 or v6) -> shell.
//  2. Otherwise, a well-formed host:port -> raw TCP (unchanged).
//  3. Otherwise (bare name, no port) -> shell.
func connectIsShellTarget(arg string) bool {
	if isValidIP(arg) {
		return true
	}
	_, port, err := parsePeerPort(arg)
	if err != nil || port == "" {
		return true
	}
	return false
}

func init() {
	rootCmd.AddCommand(connectCmd)
	connectCmd.Flags().IntVar(&connectTimeout, "timeout", 0, "Connection timeout in seconds (0 = no timeout, raw-TCP mode)")
	connectCmd.Flags().IntVar(&connectTerminalPort, "terminal-port", 7860, "Terminal endpoint port on the target (bare-target mode)")
	connectCmd.Flags().StringVar(&connectToken, "token", "", "Terminal auth token (bare-target mode; OPTIONAL, mesh identity is trusted by default; or set CITADEL_TERMINAL_TOKEN)")

	// Shared with 'citadel ssh' (same package vars, cmd/ssh.go, cmd/connect_dispatch.go)
	// so a bare target behaves identically regardless of which command name
	// was used (#754). -u/--user and -p/--port for the OpenSSH fallback are
	// deliberately NOT mirrored here: they only make sense against a real
	// sshd, and 'citadel ssh -u/-p' remains the way to set them.
	connectCmd.Flags().BoolVar(&viaRaw, "raw", false, "Force the raw sshd path, skipping the terminal endpoint entirely (bare-target mode)")
	connectCmd.Flags().BoolVar(&viaRaw, "via-sshd", false, "Alias of --raw")
	connectCmd.Flags().BoolVar(&viaMesh, "mesh", false, "Force the terminal-endpoint path only, no raw sshd fallback (bare-target mode)")
	connectCmd.Flags().BoolVar(&viaMesh, "terminal", false, "Alias of --mesh")
	connectCmd.Flags().StringVar(&shellPasscode, "passcode", "", "Node passcode for the terminal endpoint (or CITADEL_TERMINAL_PASSCODE); prompted interactively if required and omitted")
}
