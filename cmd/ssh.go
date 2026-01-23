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
	Short: "SSH to another node on the AceTeam Network",
	Long: `Establishes an SSH connection to another node on the AceTeam Network.

PEER IDENTIFICATION:
  You can specify the peer in multiple ways:
  - By hostname:  citadel ssh gpu-node-1
  - By IP:        citadel ssh 100.64.0.25
  - Interactive:  citadel ssh  (shows a list of online peers to choose from)

HOW IT WORKS:
  This command tunnels SSH through the AceTeam Network's secure mesh.
  It works even when system tools (ping, ssh) can't reach the peer directly,
  because it uses the internal tsnet userspace network.

REQUIREMENTS:
  - Both machines must be registered to the same AceTeam Network
  - The target peer must have SSH enabled (port 22 or custom port)
  - You must have valid SSH credentials for the target machine`,
	Example: `  # Interactive mode - select from available peers
  citadel ssh

  # Connect by hostname
  citadel ssh gpu-node-1

  # Connect by network IP address
  citadel ssh 100.64.0.25

  # Specify SSH username
  citadel ssh gpu-node-1 -u ubuntu

  # Specify custom SSH port
  citadel ssh gpu-node-1 -p 2222

  # Combine options
  citadel ssh gpu-node-1 -u admin -p 2222 -v`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		// Ensure network connection
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := ensureNetworkConnected(ctx); err != nil {
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

		// Resolve peer to IP
		ip, hostname, err := resolvePeer(peer)
		if err != nil {
			badColor.Printf("Could not resolve peer '%s': %v\n", peer, err)
			suggestAvailablePeers()
			os.Exit(1)
		}

		// Determine SSH port
		port := "22"
		if sshPort != "" {
			port = sshPort
		}

		// Get the path to the current citadel executable for ProxyCommand
		citadelPath, err := os.Executable()
		if err != nil {
			badColor.Printf("Could not determine citadel path: %v\n", err)
			os.Exit(1)
		}

		// Build SSH command arguments
		// Use ProxyCommand to tunnel through tsnet via 'citadel connect'
		proxyCmd := fmt.Sprintf("%s connect %s:%s", citadelPath, ip, port)
		sshArgs := []string{
			"-o", fmt.Sprintf("ProxyCommand=%s", proxyCmd),
			"-o", "StrictHostKeyChecking=accept-new", // Auto-accept new host keys (user can override)
		}

		// Add verbose flag if requested
		if sshVerbose {
			sshArgs = append(sshArgs, "-v")
		}

		// Build target - use a placeholder hostname since ProxyCommand handles the actual connection
		// SSH needs a target but ProxyCommand bypasses normal resolution
		target := ip
		if sshUser != "" {
			target = sshUser + "@" + ip
		}
		sshArgs = append(sshArgs, target)

		// Display connection info
		displayName := hostname
		if displayName == "" {
			displayName = ip
		}
		fmt.Printf("Connecting to %s via AceTeam Network...\n", displayName)
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
			badColor.Println("Error: ssh command not found. Please install OpenSSH.")
			os.Exit(1)
		}

		sshExec := exec.Command(sshPath, sshArgs...)
		sshExec.Stdin = os.Stdin
		sshExec.Stdout = os.Stdout
		sshExec.Stderr = os.Stderr

		if err := sshExec.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				os.Exit(exitErr.ExitCode())
			}
			badColor.Printf("SSH error: %v\n", err)
			os.Exit(1)
		}
	},
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
	sshCmd.Flags().StringVarP(&sshUser, "user", "u", "", "SSH username (defaults to current user)")
	sshCmd.Flags().StringVarP(&sshPort, "port", "p", "", "SSH port (defaults to 22)")
	sshCmd.Flags().BoolVarP(&sshVerbose, "verbose", "v", false, "Enable verbose SSH output")
}
