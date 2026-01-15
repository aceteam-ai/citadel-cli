// cmd/join.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
)

var (
	joinAuthkey  string
	joinNodeName string
)

var joinCmd = &cobra.Command{
	Use:   "join",
	Short: "Join the AceTeam network (lightweight, no services)",
	Long: `Connects this machine to the AceTeam network using Tailscale.
This is a lightweight command that only handles network connectivity.
No Docker installation, no services, no manifest generation.

By default, uses the system hostname as the node name and device
authorization for authentication.`,
	Example: `  # Join with defaults (hostname, device auth)
  sudo citadel join

  # Join with authkey (for automation)
  sudo citadel join --authkey tskey-auth-xxx

  # Override the node name
  sudo citadel join --node-name my-gpu-server`,
	Run: func(cmd *cobra.Command, args []string) {
		// Check for root/admin privileges
		if !platform.IsRoot() {
			if platform.IsWindows() {
				fmt.Fprintln(os.Stderr, "Error: join command must be run as Administrator.")
			} else {
				fmt.Fprintln(os.Stderr, "Error: join command must be run with sudo.")
			}
			os.Exit(1)
		}

		// Check if already connected
		if nexus.IsTailscaleConnected() {
			fmt.Println("Already connected to the AceTeam network.")
			return
		}

		// Ensure Tailscale is installed
		if err := ensureTailscaleInstalled(); err != nil {
			fmt.Fprintf(os.Stderr, "Error installing Tailscale: %v\n", err)
			os.Exit(1)
		}

		// Get node name (default to hostname)
		nodeName := joinNodeName
		if nodeName == "" {
			hostname, err := os.Hostname()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: could not determine hostname: %v\n", err)
				os.Exit(1)
			}
			nodeName = hostname
		}

		// Get authkey (device auth if not provided)
		authkeyToUse := joinAuthkey
		if authkeyToUse == "" {
			// Run device authorization flow
			token, err := runDeviceAuthFlow(authServiceURL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				fmt.Fprintf(os.Stderr, "\nAlternative: Get an authkey at %s/fabric\n", authServiceURL)
				fmt.Fprintln(os.Stderr, "Then run: sudo citadel join --authkey <your-key>")
				os.Exit(1)
			}
			authkeyToUse = token.Authkey
		}

		// Join the network
		fmt.Printf("Joining network as '%s'...\n", nodeName)
		if err := joinTailscaleNetwork(nodeName, authkeyToUse); err != nil {
			fmt.Fprintf(os.Stderr, "Error joining network: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("\nSuccessfully joined the AceTeam network!")
		fmt.Printf("Node name: %s\n", nodeName)
	},
}

// ensureTailscaleInstalled checks if Tailscale is installed and installs it if not
func ensureTailscaleInstalled() error {
	// Check if tailscale command exists
	if _, err := exec.LookPath("tailscale"); err == nil {
		return nil // Already installed
	}

	fmt.Println("Installing Tailscale...")

	if platform.IsWindows() {
		cmd := exec.Command("winget", "install", "--id", "Tailscale.Tailscale", "--silent", "--accept-package-agreements", "--accept-source-agreements")
		output, err := cmd.CombinedOutput()
		if err != nil {
			outputStr := string(output)
			if strings.Contains(outputStr, "already installed") ||
				strings.Contains(outputStr, "No applicable upgrade found") ||
				strings.Contains(outputStr, "No available upgrade found") {
				return nil // Already installed
			}
			return fmt.Errorf("winget install failed: %w", err)
		}

		// Start the Tailscale service
		exec.Command("net", "start", "Tailscale").Run()
		return nil
	}

	// Linux/macOS: Use the official install script
	script := "curl -fsSL https://tailscale.com/install.sh | sh"
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// joinTailscaleNetwork connects to the Tailscale network with the given credentials
func joinTailscaleNetwork(nodeName, authkey string) error {
	// Logout first (ignore errors)
	if platform.IsWindows() {
		exec.Command("tailscale", "logout").Run()
	} else {
		exec.Command("sudo", "tailscale", "logout").Run()
	}

	// Build the tailscale up command
	var tsCmd *exec.Cmd
	if platform.IsWindows() {
		tsCmd = exec.Command("tailscale", "up",
			"--login-server="+nexusURL,
			"--authkey="+authkey,
			"--hostname="+nodeName,
			"--accept-routes",
			"--accept-dns",
		)
	} else {
		tsCmd = exec.Command("sudo", "tailscale", "up",
			"--login-server="+nexusURL,
			"--authkey="+authkey,
			"--hostname="+nodeName,
			"--accept-routes",
			"--accept-dns",
		)
	}

	output, err := tsCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tailscale up failed: %s", string(output))
	}
	return nil
}

func init() {
	rootCmd.AddCommand(joinCmd)
	joinCmd.Flags().StringVar(&joinAuthkey, "authkey", "", "Pre-generated authkey for non-interactive join")
	joinCmd.Flags().StringVar(&joinNodeName, "node-name", "", "Override the node name (defaults to hostname)")
}
