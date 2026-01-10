// cmd/login.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/aceboss/citadel-cli/internal/nexus"
	"github.com/aceboss/citadel-cli/internal/ui"
	"github.com/spf13/cobra"
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate this machine with the AceTeam Nexus",
	Long: `Connects this machine to your AceTeam network. If already connected, it does
nothing. Otherwise, it interactively prompts for an authentication method.
This command may require sudo to modify system network settings.`,
	Run: func(cmd *cobra.Command, args []string) {
		// The login command doesn't have an authkey flag, so we pass an empty string.
		choice, key, err := nexus.GetNetworkChoice("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Canceled: %v\n", err)
			os.Exit(1)
		}

		var tsCmd *exec.Cmd

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
				fmt.Fprintln(os.Stderr, "\nAlternative: Use 'citadel login' and select authkey option")
				os.Exit(1)
			}

			// Get node name
			suggestedHostname, err := os.Hostname()
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Could not determine system hostname: %v\n", err)
			}

			nodeName, err := ui.AskInput("Enter a name for this node:", "e.g., my-laptop", suggestedHostname)
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Could not determine node name: %v\n", err)
				os.Exit(1)
			}

			// Use the token as authkey
			exec.Command("sudo", "tailscale", "logout").Run()
			tsCmd = exec.Command("sudo", "tailscale", "up",
				"--login-server="+nexusURL,
				"--authkey="+token.Authkey,
				"--hostname="+nodeName,
				"--accept-routes",
				"--accept-dns",
			)
		case nexus.NetChoiceAuthkey:
			fmt.Println("--- Authenticating with authkey ---")
			fmt.Println("   - Using system hostname as node name.")
			suggestedHostname, err := os.Hostname()
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Could not determine system hostname: %v\n", err)
			}

			nodeName, err := ui.AskInput("Enter a name for this node:", "e.g., my-laptop", suggestedHostname)
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Could not determine node name: %v\n", err)
				os.Exit(1)
			}
			exec.Command("sudo", "tailscale", "logout").Run()
			tsCmd = exec.Command("sudo", "tailscale", "up",
				"--login-server="+nexusURL,
				"--authkey="+key,
				"--hostname="+nodeName,
				"--accept-routes",
				"--accept-dns",
			)
		}

		tsCmd.Stdout = os.Stdout
		tsCmd.Stderr = os.Stderr
		tsCmd.Stdin = os.Stdin

		if err := tsCmd.Run(); err != nil {
			os.Exit(1)
		}

		fmt.Println("\n✅ Authentication successful! This machine is now connected to the fabric.")
	},
}

func init() {
	rootCmd.AddCommand(loginCmd)
}
