// cmd/ssh.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
	"github.com/spf13/cobra"
)

var (
	sshUser    string
	sshPort    string
	sshVerbose bool
)

var sshCmd = &cobra.Command{
	Use:   "ssh [peer]",
	Short: "Open a shell on another node on the AceTeam Network",
	Long: `Opens a shell on another node on the AceTeam Network.

This command is an alias of 'citadel connect <peer>' (see
'citadel connect --help'): a bare peer target routes through the exact same
logic on both commands, so 'citadel ssh <node>' and 'citadel connect <node>'
behave identically (issue #754).

PEER IDENTIFICATION:
  You can specify the peer in multiple ways:
  - By hostname:  citadel ssh gpu-node-1
  - By IP:        citadel ssh 100.64.0.25
  - Interactive:  citadel ssh  (shows a list of online peers to choose from)

HOW IT WORKS:
  By default this tries the AceTeam Network terminal endpoint first (works on
  every node, including nodes whose host sshd is not exposed on the mesh, no
  host SSH config required). A node passcode challenge is NOT treated as
  a failure to fall back on: you are prompted for it interactively instead.
  Only when the terminal endpoint itself is unreachable does this fall back
  to a real OpenSSH connection, tunneled through the mesh to the peer's sshd
  (port 22 by default).

  The terminal endpoint gives you a PLAIN shell by default: no tmux, nothing
  persists once you disconnect. Pass --tmux to opt into a persistent,
  reconnect-resilient session instead, a repeated connect (or a reconnect
  after a drop) re-attaches to the same live shell, running command and
  scrollback intact. If the target has no tmux binary, it is installed on
  the node automatically; if that install fails, you get a plain shell with
  a warning instead of a failed connection. --no-tmux is accepted for
  symmetry but is already the default.

  --raw (alias --via-sshd) skips straight to the OpenSSH path, no terminal
  endpoint attempt at all.
  --mesh (alias --terminal) forces the terminal-endpoint path only, with no
  OpenSSH fallback.
  -u/--user and -p/--port select the OpenSSH path implicitly, since they only
  make sense against a real sshd.

REQUIREMENTS:
  - Both machines must be registered to the same AceTeam Network
  - For the OpenSSH path: the target peer must have SSH enabled (port 22 or
    a custom port) and you must have valid SSH credentials for it`,
	Example: `  # Interactive mode - select from available peers
  citadel ssh

  # Connect by hostname (tries the terminal endpoint, falls back to sshd)
  citadel ssh gpu-node-1

  # Connect by network IP address
  citadel ssh 100.64.0.25

  # Force the OpenSSH path with a specific user and port
  citadel ssh gpu-node-1 -u ubuntu -p 2222

  # Force the terminal-endpoint path only, no OpenSSH fallback
  citadel ssh gpu-node-1 --mesh

  # Opt into a persistent, reconnect-resilient tmux-backed session
  citadel ssh gpu-node-1 --tmux

  # Supply a node passcode up front instead of being prompted
  citadel ssh gpu-node-1 --passcode 123456`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		// Ensure network connection (also needed for the interactive picker
		// below, before connectToNode gets a chance to ensure it itself).
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := ensureNetworkConnectedFn(ctx); err != nil {
			badColor.Println(err)
			os.Exit(1)
		}

		var peer string

		// Interactive mode if no peer specified
		if len(args) == 0 {
			selectedPeer, err := selectPeerInteractive(ctx)
			if err != nil {
				badColor.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			peer = selectedPeer
		} else {
			peer = args[0]
		}

		if err := connectToNode(cmd, peer); err != nil {
			badColor.Printf("Error: %v\n", err)
			os.Exit(1)
		}
	},
}

// runLegacySSH execs the real ssh(1) binary against peer, tunneled through
// the mesh via 'citadel connect <ip>:<port>' as ProxyCommand. This predates
// #754's ts-net-first routing and is now: the fallback path when the ts-net
// terminal endpoint is unreachable, and the explicit path for
// --raw/--via-sshd (or naming -u/-p, which only make sense against a real
// sshd).
func runLegacySSH(peer string) error {
	ip, hostname, err := resolvePeer(peer)
	if err != nil {
		suggestAvailablePeers()
		return fmt.Errorf("could not resolve peer '%s': %w", peer, err)
	}

	// Determine SSH port
	port := "22"
	if sshPort != "" {
		port = sshPort
	}

	// Get the path to the current citadel executable for ProxyCommand
	citadelPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine citadel path: %w", err)
	}

	sshArgs := buildSSHArgs(citadelPath, ip, port, sshUser, sshVerbose)

	// Display connection info
	displayName := hostname
	if displayName == "" {
		displayName = ip
	}
	fmt.Printf("Connecting to %s via AceTeam Network (raw sshd)...\n", displayName)
	if hostname != "" && hostname != ip {
		fmt.Printf("  Peer: %s (%s)\n", hostname, ip)
	}
	if sshUser != "" {
		fmt.Printf("  User: %s\n", sshUser)
	}
	if port != "22" {
		fmt.Printf("  Port: %s\n", port)
	}
	fmt.Println()

	// Execute SSH
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh command not found. Please install OpenSSH")
	}

	sshExec := exec.Command(sshPath, sshArgs...)
	sshExec.Stdin = os.Stdin
	sshExec.Stdout = os.Stdout
	sshExec.Stderr = os.Stderr

	if err := sshExec.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("ssh error: %w", err)
	}
	return nil
}

// buildSSHArgs constructs the argv passed to the ssh(1) binary for the raw
// sshd path (#754's fallback / --raw path). Factored out for testability: it
// takes only the exact values that end up in argv, so a test can pin exactly
// what ssh(1) sees. Notably it has no passcode parameter at all: the node
// passcode (citadel#753) belongs to the terminal-endpoint auth path, which
// this raw-sshd path never touches, and it must never appear in a
// subprocess argv (visible to other users on the same machine via `ps`).
func buildSSHArgs(citadelPath, ip, port, user string, verbose bool) []string {
	proxyCmd := fmt.Sprintf("%s connect %s:%s", citadelPath, ip, port)
	args := []string{
		"-o", fmt.Sprintf("ProxyCommand=%s", proxyCmd),
		"-o", "StrictHostKeyChecking=accept-new", // Auto-accept new host keys (user can override)
	}
	if verbose {
		args = append(args, "-v")
	}
	// Build target - use a placeholder hostname since ProxyCommand handles the actual connection
	// SSH needs a target but ProxyCommand bypasses normal resolution
	target := ip
	if user != "" {
		target = user + "@" + ip
	}
	return append(args, target)
}

// selectPeerInteractive shows a list of online peers and lets the user select one.
func selectPeerInteractive(ctx context.Context) (string, error) {
	// Get our own IP to filter ourselves out
	myIP, _ := network.GetGlobalIPv4()

	// Get peers
	peers, err := network.GetGlobalPeers(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get peers: %w", err)
	}

	// Filter to online peers (excluding ourselves)
	var choices []string
	var peerMap = make(map[string]string) // display -> hostname

	for _, peer := range peers {
		if peer.IP != "" && peer.IP != myIP && peer.Online {
			display := fmt.Sprintf("%s (%s)", peer.Hostname, peer.IP)
			if peer.OS != "" {
				display = fmt.Sprintf("%s (%s) [%s]", peer.Hostname, peer.IP, peer.OS)
			}
			choices = append(choices, display)
			peerMap[display] = peer.Hostname
		}
	}

	if len(choices) == 0 {
		return "", fmt.Errorf("no online peers found on the network")
	}

	// Show interactive selection
	fmt.Println("Select a peer to connect to:")
	fmt.Println()

	selected, err := ui.AskSelect("Available peers:", choices)
	if err != nil {
		return "", err
	}

	return peerMap[selected], nil
}

func init() {
	rootCmd.AddCommand(sshCmd)
	sshCmd.Flags().StringVarP(&sshUser, "user", "u", "", "SSH username for the raw sshd path (implies --raw)")
	sshCmd.Flags().StringVarP(&sshPort, "port", "p", "", "sshd port for the raw sshd path, default 22 (implies --raw)")
	sshCmd.Flags().BoolVarP(&sshVerbose, "verbose", "v", false, "Enable verbose OpenSSH output (raw sshd path only)")
	sshCmd.Flags().BoolVar(&viaRaw, "raw", false, "Force the raw sshd path, skipping the terminal endpoint entirely")
	sshCmd.Flags().BoolVar(&viaRaw, "via-sshd", false, "Alias of --raw")
	sshCmd.Flags().BoolVar(&viaMesh, "mesh", false, "Force the terminal-endpoint path only, no raw sshd fallback")
	sshCmd.Flags().BoolVar(&viaMesh, "terminal", false, "Alias of --mesh")
	sshCmd.Flags().StringVar(&shellPasscode, "passcode", "", "Node passcode for the terminal endpoint (or CITADEL_TERMINAL_PASSCODE); prompted interactively if required and omitted")
	sshCmd.Flags().BoolVar(&wantTmuxSession, "tmux", false, "Opt into a persistent, reconnect-resilient tmux-backed session (terminal-endpoint path only; default is a plain shell)")
	sshCmd.Flags().BoolVar(&wantNoTmuxSession, "no-tmux", false, "Explicit bare shell (the default); accepted for symmetry with --tmux")
}
