// cmd/login.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
	"github.com/spf13/cobra"
)

var (
	loginAuthkey  string
	loginNodeName string
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate this machine with the AceTeam Network",
	Long: `Connects this machine to your AceTeam network. If already connected, it does
nothing. Otherwise, it interactively prompts for an authentication method.

Use --authkey for non-interactive authentication (ideal for automation).`,
	Example: `  # Interactive login (prompts for auth method)
  citadel login

  # Non-interactive login with authkey (for automation)
  citadel login --authkey tskey-auth-xxx

  # Override the node name
  citadel login --authkey tskey-auth-xxx --node-name my-gpu-server`,
	Run: func(cmd *cobra.Command, args []string) {
		// Non-interactive mode when authkey is provided
		if loginAuthkey != "" {
			runNonInteractiveLogin()
			return
		}

		// Interactive mode (existing behavior)
		runInteractiveLogin()
	},
}

// runNonInteractiveLogin handles login with --authkey flag (formerly 'citadel join')
func runNonInteractiveLogin() {
	// Check if already connected
	if network.IsGlobalConnected() {
		fmt.Println("Already connected to the AceTeam Network.")
		return
	}

	// Get node name (default to hostname)
	nodeName := loginNodeName
	if nodeName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: could not determine hostname: %v\n", err)
			os.Exit(1)
		}
		nodeName = hostname
	}

	// Connect to the network
	fmt.Printf("Connecting to AceTeam Network as '%s'...\n", nodeName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	config := network.ServerConfig{
		Hostname:   nodeName,
		ControlURL: nexusURL, // From root.go
		AuthKey:    loginAuthkey,
	}

	srv, err := network.Connect(ctx, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to network: %v\n", err)
		os.Exit(1)
	}

	ip, _ := srv.GetIPv4()
	fmt.Println("\n✅ Successfully connected to the AceTeam Network!")
	fmt.Printf("   Node: %s\n", nodeName)
	if ip != "" {
		fmt.Printf("   IP: %s\n", ip)
	}
}

// runInteractiveLogin handles the interactive login flow
func runInteractiveLogin() {
	choice, key, err := nexus.GetNetworkChoice("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Canceled: %v\n", err)
		os.Exit(1)
	}

	var authKey string
	var nodeName string

	switch choice {
	case nexus.NetChoiceVerified:
		// The GetNetworkChoice function already printed a success message.
		return
	case nexus.NetChoiceSkip:
		fmt.Println("Login skipped.")
		return
	case nexus.NetChoiceDevice:
		// Device authorization flow
		token, err := runDeviceAuthFlow(authServiceURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			fmt.Fprintln(os.Stderr, "\nAlternative: Use 'citadel login --authkey <key>' for non-interactive login")
			os.Exit(1)
		}
		authKey = token.Authkey

		// Get node name
		suggestedHostname, _ := os.Hostname()
		nodeName, err = ui.AskInput("Enter a name for this node:", "e.g., my-laptop", suggestedHostname)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Could not determine node name: %v\n", err)
			os.Exit(1)
		}

	case nexus.NetChoiceAuthkey:
		fmt.Println("--- Authenticating with authkey ---")
		authKey = key

		suggestedHostname, _ := os.Hostname()
		nodeName, err = ui.AskInput("Enter a name for this node:", "e.g., my-laptop", suggestedHostname)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Could not determine node name: %v\n", err)
			os.Exit(1)
		}
	}

	// Disconnect any existing connection first
	_ = network.Logout()

	// Connect using tsnet
	fmt.Printf("Connecting to AceTeam Network as '%s'...\n", nodeName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	config := network.ServerConfig{
		Hostname:   nodeName,
		ControlURL: nexusURL,
		AuthKey:    authKey,
	}

	srv, err := network.Connect(ctx, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to connect: %v\n", err)
		os.Exit(1)
	}

	ip, _ := srv.GetIPv4()
	fmt.Println("\n✅ Authentication successful! This machine is now connected to the AceTeam Network.")
	if ip != "" {
		fmt.Printf("   IP: %s\n", ip)
	}
}

func init() {
	rootCmd.AddCommand(loginCmd)
	loginCmd.Flags().StringVar(&loginAuthkey, "authkey", "", "Pre-generated authkey for non-interactive login")
	loginCmd.Flags().StringVar(&loginNodeName, "node-name", "", "Override the node name (defaults to hostname)")
}
