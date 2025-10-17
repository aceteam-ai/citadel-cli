// cmd/login.go
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
	Short: "Authenticate this machine with the AceTeam Nexus",
	Long: `This command authenticates the Citadel node with your AceTeam account.
It will trigger a device-based OAuth flow, asking you to open a browser
and log in to authorize this machine to join your private network.
This command requires sudo to modify system network settings.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("--- Starting AceTeam Authentication ---")
		fmt.Println("This command requires root privileges to configure the network.")
		fmt.Println("Please follow the instructions in your browser to complete login.")

		// This is the correct way to wrap a command that needs privilege escalation.
		tailscaleCmd := exec.Command("sudo", "tailscale", "login", "--login-server="+nexusURL)

		// Pipe the command's output directly to our terminal so the user can see it
		tailscaleCmd.Stdout = os.Stdout
		tailscaleCmd.Stderr = os.Stderr
		// Also pipe Stdin to handle potential password prompts from sudo
		tailscaleCmd.Stdin = os.Stdin

		// Run the command
		err := tailscaleCmd.Run()
		if err != nil {
			// The error message from tailscale is usually informative enough.
			// We don't need to print our own generic error.
			os.Exit(1)
		}

		fmt.Println("\n✅ Authentication successful! This machine can now join the fabric.")
	},
}

func init() {
	rootCmd.AddCommand(loginCmd)
}
