// cmd/logout.go
package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodesession"
	"github.com/spf13/cobra"
)

var (
	logoutKeepRegistration  bool
	logoutForce             bool
	logoutAPIKeyStdin       bool
	logoutRequireDeregister bool
)

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Disconnect and deregister this machine from the AceTeam Network",
	Long: `Disconnects this machine from your AceTeam network and deregisters it from
the coordination server. This allows the node to be re-registered under a
different organization when running 'citadel init' again.

By default, logout will:
  1. Deregister the node from Headscale (allows re-registration to different org)
  2. Disconnect from the network
  3. Clear local network state

Use --keep-registration to only disconnect locally without deregistering from
Headscale. This is useful for temporary disconnects where you want to reconnect
to the same organization later.`,
	Run: runLogout,
}

func runLogout(cmd *cobra.Command, args []string) {
	if logoutKeepRegistration && (logoutAPIKeyStdin || logoutRequireDeregister) {
		fmt.Fprintln(os.Stderr, "Error: --keep-registration cannot be combined with strict deregistration options")
		os.Exit(1)
	}
	// Check if connected or has state
	if !network.IsGlobalConnected() && !network.HasState() && !(logoutRequireDeregister && getSavedHostname() != "") {
		fmt.Println("Not connected to any network.")
		return
	}

	// Confirm before disconnecting
	if !logoutForce {
		action := "Disconnect and deregister from the AceTeam Network?"
		if logoutKeepRegistration {
			action = "Disconnect from the AceTeam Network?"
		}
		fmt.Printf("%s (y/N) ", action)
		var response string
		fmt.Scanln(&response)
		response = strings.TrimSpace(strings.ToLower(response))
		if response != "y" && response != "yes" {
			fmt.Println("Aborted.")
			return
		}
	}

	fmt.Println("--- Disconnecting from the AceTeam Network ---")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	apiKey := os.Getenv("CITADEL_API_KEY")
	if logoutAPIKeyStdin {
		var err error
		apiKey, err = readLogoutAPIKeyStdin(cmd.InOrStdin())
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading --api-key-stdin: %v\n", err)
			os.Exit(1)
		}
	}

	// Try to deregister from backend (unless --keep-registration is set)
	var deregisterErr error
	if !logoutKeepRegistration {
		deregisterErr = deregisterFromBackend(ctx, apiKey)
		if deregisterErr != nil && !logoutRequireDeregister {
			fmt.Printf("   - Warning: Could not deregister from backend: %v\n", deregisterErr)
			fmt.Println("   - Node may still be registered in Headscale")
		}
	} else {
		Debug("skipping backend deregistration (--keep-registration)")
	}

	// Logout (disconnect and clear state)
	if err := network.Logout(); err != nil {
		fmt.Fprintf(os.Stderr, "Error logging out: %v\n", err)
		os.Exit(1)
	}
	if !logoutKeepRegistration {
		// A later enrollment is a new identity and must receive its own
		// authkey/device-auth default, not inherit this account's mode.
		if err := os.Remove(nodesession.Path(network.GetNodeConfigDir())); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Warning: could not clear prior session mode: %v\n", err)
		}
	}
	if deregisterErr != nil && logoutRequireDeregister {
		fmt.Fprintf(os.Stderr, "Error: required backend deregistration failed: %v\n", deregisterErr)
		os.Exit(1)
	}

	fmt.Println("✅ Successfully disconnected from the AceTeam Network.")
	if logoutKeepRegistration {
		fmt.Println("   Node registration preserved. To reconnect, run 'citadel login'")
	} else {
		fmt.Println("   To register again, run 'citadel init'")
	}
}

// deregisterFromBackend deregisters the node via the backend API. Its caller
// decides whether a failure is a warning or a hard cleanup failure.
func deregisterFromBackend(ctx context.Context, apiKey string) error {
	var nodeName string

	// Try to get node identity from manifest
	if manifest, _, err := findAndReadManifest(); err == nil {
		nodeName = manifest.Node.Name
		Debug("from manifest: nodeName=%s", nodeName)
	}

	// Try to get more accurate hostname from network status (if connected)
	if status, err := network.GetGlobalStatus(ctx); err == nil && status.Connected {
		if status.Hostname != "" {
			Debug("from network status: hostname=%s (overriding manifest)", status.Hostname)
			nodeName = status.Hostname
		}
	}
	// A CSR-enrolled login serves as node-{uid}. The manifest keeps the
	// display name, so an offline logout must resolve the signed serving
	// identity before asking the backend to deregister that exact node.
	nodeName = logoutServingNodeName(nodeName)
	if nodeName == "" {
		nodeName = getSavedHostname()
	}

	// Skip if we have no identity information
	if nodeName == "" {
		return fmt.Errorf("no node identity found")
	}

	if err := requireBackendDeregistration(ctx, nodeName, apiKey, func(ctx context.Context, nodeName, apiKey string) error {
		Debug("deregistering from backend: nodeName=%s", nodeName)
		client := nexus.NewDeregisterClient(authServiceURL, apiKey)
		return client.Deregister(ctx, nexus.DeregisterRequest{NodeName: nodeName})
	}); err != nil {
		return err
	}
	fmt.Println("   - Deregistered from coordination server")
	return nil
}

func requireBackendDeregistration(ctx context.Context, nodeName, apiKey string, deregister func(context.Context, string, string) error) error {
	if nodeName == "" {
		return fmt.Errorf("no node identity found")
	}
	if apiKey == "" {
		return fmt.Errorf("no API key configured")
	}
	if err := deregister(ctx, nodeName, apiKey); err != nil {
		return fmt.Errorf("coordination server did not confirm deregistration: %w", err)
	}
	return nil
}

const maxStdinAPIKeyBytes = 4096

func readLogoutAPIKeyStdin(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxStdinAPIKeyBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxStdinAPIKeyBytes {
		return "", fmt.Errorf("input exceeds %d bytes", maxStdinAPIKeyBytes)
	}
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
		if len(b) > 0 && b[len(b)-1] == '\r' {
			b = b[:len(b)-1]
		}
	}
	if len(b) == 0 {
		return "", fmt.Errorf("input is empty")
	}
	for _, c := range b {
		if c < 0x21 || c > 0x7e {
			return "", fmt.Errorf("input must be one visible ASCII token")
		}
	}
	return string(b), nil
}

func logoutServingNodeName(fallback string) string {
	return servingIdentityHostname(loadLoginNodeUID(), fallback)
}

func init() {
	rootCmd.AddCommand(logoutCmd)

	logoutCmd.Flags().BoolVar(&logoutKeepRegistration, "keep-registration", false,
		"Only disconnect locally, keep node registered in Headscale (for temporary disconnects)")
	logoutCmd.Flags().BoolVarP(&logoutForce, "force", "f", false, "Skip confirmation prompt.")
	logoutCmd.Flags().BoolVar(&logoutAPIKeyStdin, "api-key-stdin", false, "Read the backend API key from stdin (keeps it out of argv)")
	logoutCmd.Flags().BoolVar(&logoutRequireDeregister, "require-deregister", false, "Fail unless backend deregistration is confirmed")
}
