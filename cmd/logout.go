// cmd/logout.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/aceboss/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
)

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Disconnect this machine from the AceTeam Nexus",
	Long: `Disconnects this machine from your AceTeam network by logging out of Tailscale.
This command will stop the node from receiving jobs and remove it from the network
until you run 'citadel login' again. This command requires sudo to modify system
network settings (or Administrator privileges on Windows).`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("--- Disconnecting from the AceTeam network ---")

		tailscaleCLI := getTailscaleCLI()

		// Check if tailscale is running
		statusCmd := exec.Command(tailscaleCLI, "status")
		if err := statusCmd.Run(); err != nil {
			fmt.Println("⚠️  Tailscale daemon is not running or not installed.")
			fmt.Println("You may already be disconnected from the network.")
			return
		}

		// Run tailscale logout (with sudo on Linux/macOS, directly on Windows)
		var logoutExec *exec.Cmd
		if platform.IsWindows() {
			logoutExec = exec.Command(tailscaleCLI, "logout")
		} else {
			logoutExec = exec.Command("sudo", tailscaleCLI, "logout")
		}
		logoutExec.Stdout = os.Stdout
		logoutExec.Stderr = os.Stderr

		if err := logoutExec.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error logging out: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("✅ Successfully disconnected from the AceTeam network.")
		fmt.Println("   To reconnect, run 'citadel login' or 'citadel up --authkey <key>'")
	},
}

func init() {
	rootCmd.AddCommand(logoutCmd)
}
