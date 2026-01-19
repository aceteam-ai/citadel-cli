// cmd/logout.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/spf13/cobra"
)

var (
	logoutKeepRegistration bool
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
	fmt.Println("--- Disconnecting from the AceTeam Network ---")

	// Check if connected or has state
	if !network.IsGlobalConnected() && !network.HasState() {
		fmt.Println("Not connected to any network.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Try to deregister from backend (unless --keep-registration is set)
	if !logoutKeepRegistration {
		deregisterFromBackend(ctx)
	} else {
		Debug("skipping backend deregistration (--keep-registration)")
	}

	// Logout (disconnect and clear state)
	if err := network.Logout(); err != nil {
		fmt.Fprintf(os.Stderr, "Error logging out: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✅ Successfully disconnected from the AceTeam Network.")
	if logoutKeepRegistration {
		fmt.Println("   Node registration preserved. To reconnect, run 'citadel login'")
	} else {
		fmt.Println("   To register again, run 'citadel init'")
	}
}

// deregisterFromBackend attempts to deregister the node from Headscale via the backend API.
// This is a best-effort operation - errors are logged but don't block logout.
func deregisterFromBackend(ctx context.Context) {
	var orgID, nodeName string

	// Try to get node identity from manifest
	if manifest, _, err := findAndReadManifest(); err == nil {
		orgID = manifest.Node.OrgID
		nodeName = manifest.Node.Name
		Debug("from manifest: orgID=%s, nodeName=%s", orgID, nodeName)
	}

	// Try to get more accurate hostname from network status (if connected)
	if status, err := network.GetGlobalStatus(ctx); err == nil && status.Connected {
		if status.Hostname != "" {
			Debug("from network status: hostname=%s (overriding manifest)", status.Hostname)
			nodeName = status.Hostname
		}
	}

	// Skip if we have no identity information
	if orgID == "" && nodeName == "" {
		Debug("no node identity found, skipping deregistration")
		return
	}

	// Call backend to deregister
	Debug("deregistering from backend: orgID=%s, nodeName=%s", orgID, nodeName)
	client := nexus.NewDeregisterClient(authServiceURL)
	req := nexus.DeregisterRequest{
		OrgID:    orgID,
		NodeName: nodeName,
	}

	if err := client.Deregister(ctx, req); err != nil {
		// Warning only - continue with local logout
		fmt.Printf("   - Warning: Could not deregister from backend: %v\n", err)
		fmt.Println("   - Node may still be registered in Headscale")
	} else {
		fmt.Println("   - Deregistered from coordination server")
	}
}

func init() {
	rootCmd.AddCommand(logoutCmd)

	logoutCmd.Flags().BoolVar(&logoutKeepRegistration, "keep-registration", false,
		"Only disconnect locally, keep node registered in Headscale (for temporary disconnects)")
}
