// cmd/logout.go
package cmd

import (
	"fmt"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/spf13/cobra"
)

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Disconnect this machine from the AceTeam Network",
	Long: `Disconnects this machine from your AceTeam network. This command will stop
the node from receiving jobs and remove it from the network until you run
'citadel login' again.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("--- Disconnecting from the AceTeam Network ---")

		// Check if connected
		if !network.IsGlobalConnected() && !network.HasState() {
			fmt.Println("Not connected to any network.")
			return
		}

		// Logout (disconnect and clear state)
		if err := network.Logout(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error logging out: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("✅ Successfully disconnected from the AceTeam Network.")
		fmt.Println("   To reconnect, run 'citadel login' or 'citadel login --authkey <key>'")
	},
}

func init() {
	rootCmd.AddCommand(logoutCmd)
}
