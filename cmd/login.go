package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// loginCmd represents the login command
var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate the CLI with the AceTeam Nexus",
	Long: `This command authenticates the Citadel CLI with your AceTeam account.
It will trigger a device-based OAuth flow, asking you to open a browser
and log in to authorize this machine to join your private network.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("--- Starting AceTeam Authentication ---")
		fmt.Println("Please follow the instructions in your browser to complete login.")

		// Construct the tailscale command
		tailscaleCmd := exec.Command("tailscale", "login", "--login-server="+nexusURL)

		// Pipe the command's output directly to our terminal so the user can see it
		tailscaleCmd.Stdout = os.Stdout
		tailscaleCmd.Stderr = os.Stderr

		// Run the command
		err := tailscaleCmd.Run()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error running tailscale login: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("\n✅ Authentication successful! This machine can now join the fabric.")
	},
}

func init() {
	rootCmd.AddCommand(loginCmd)

}
