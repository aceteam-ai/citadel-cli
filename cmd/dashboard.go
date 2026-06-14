// cmd/dashboard.go
package cmd

import (
	"fmt"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/tui"
	"github.com/aceteam-ai/citadel-cli/internal/tui/dashboard"
	"github.com/spf13/cobra"
)

var dashboardCmd = &cobra.Command{
	Use:     "dashboard",
	Aliases: []string{"dash"},
	Short:   "Launch the interactive TUI dashboard",
	Long: `Launches a full-screen interactive dashboard showing real-time node status,
system resources, GPU utilization, network peers, and managed services.

The dashboard auto-refreshes every 5 seconds. Press 'r' to refresh manually,
'a' to toggle auto-refresh, or 'q' to quit.`,
	Example: `  # Launch the dashboard
  citadel dashboard

  # Short alias
  citadel dash`,
	Run: func(cmd *cobra.Command, args []string) {
		if !tui.IsTTY() {
			fmt.Fprintln(os.Stderr, "Dashboard requires a terminal. Use 'citadel status' or 'citadel status --json' instead.")
			os.Exit(1)
		}

		data, err := gatherStatusData()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to gather status data: %v\n", err)
			os.Exit(1)
		}

		if err := dashboard.RunDashboard(data, gatherStatusData); err != nil {
			fmt.Fprintf(os.Stderr, "Dashboard error: %v\n", err)
			os.Exit(1)
		}
	},
}

func init() {
	rootCmd.AddCommand(dashboardCmd)
}
