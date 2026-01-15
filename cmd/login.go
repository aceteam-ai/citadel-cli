// cmd/login.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
	"github.com/spf13/cobra"
)

var (
	loginAuthkey  string
	loginNodeName string
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate this machine with the AceTeam Nexus",
	Long: `Connects this machine to your AceTeam network. If already connected, it does
nothing. Otherwise, it interactively prompts for an authentication method.

Use --authkey for non-interactive authentication (ideal for automation).
This command may require sudo to modify system network settings.`,
	Example: `  # Interactive login (prompts for auth method)
  sudo citadel login

  # Non-interactive login with authkey (for automation)
  sudo citadel login --authkey tskey-auth-xxx

  # Override the node name
  sudo citadel login --authkey tskey-auth-xxx --node-name my-gpu-server`,
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
	// Check for root/admin privileges
	if !platform.IsRoot() {
		if platform.IsWindows() {
			fmt.Fprintln(os.Stderr, "Error: login command must be run as Administrator.")
		} else {
			fmt.Fprintln(os.Stderr, "Error: login command must be run with sudo.")
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
	nodeName := loginNodeName
	if nodeName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: could not determine hostname: %v\n", err)
			os.Exit(1)
		}
		nodeName = hostname
	}

	// Join the network
	fmt.Printf("Joining network as '%s'...\n", nodeName)
	if err := joinTailscaleNetwork(nodeName, loginAuthkey); err != nil {
		fmt.Fprintf(os.Stderr, "Error joining network: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\n✅ Successfully joined the AceTeam network!")
	fmt.Printf("Node name: %s\n", nodeName)
}

// runInteractiveLogin handles the interactive login flow
func runInteractiveLogin() {
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
			fmt.Fprintln(os.Stderr, "\nAlternative: Use 'citadel login --authkey <key>' for non-interactive login")
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
		tailscaleLogout()
		tsCmd = tailscaleUpCommand(nodeName, token.Authkey)
	case nexus.NetChoiceAuthkey:
		fmt.Println("--- Authenticating with authkey ---")
		suggestedHostname, err := os.Hostname()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Could not determine system hostname: %v\n", err)
		}

		nodeName, err := ui.AskInput("Enter a name for this node:", "e.g., my-laptop", suggestedHostname)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Could not determine node name: %v\n", err)
			os.Exit(1)
		}
		tailscaleLogout()
		tsCmd = tailscaleUpCommand(nodeName, key)
	}

	tsCmd.Stdout = os.Stdout
	tsCmd.Stderr = os.Stderr
	tsCmd.Stdin = os.Stdin

	if err := tsCmd.Run(); err != nil {
		os.Exit(1)
	}

	fmt.Println("\n✅ Authentication successful! This machine is now connected to the fabric.")
}

// getTailscalePath returns the path to the tailscale CLI binary.
// Returns empty string if Tailscale is not installed.
// Delegates to platform.GetTailscaleCLI() for the actual path resolution.
func getTailscalePath() string {
	if !platform.IsTailscaleInstalled() {
		return ""
	}
	return platform.GetTailscaleCLI()
}

// ensureTailscaleInstalled checks if Tailscale is installed and installs it if not.
// After installation, it verifies that the CLI is functional (important for macOS
// App Store installations where CLI must be enabled in Settings).
func ensureTailscaleInstalled() error {
	// Check if tailscale is available (PATH or macOS App Store)
	if getTailscalePath() != "" {
		// Verify CLI is functional (may require manual enabling on macOS App Store)
		if err := platform.CheckTailscaleCLIEnabled(); err != nil {
			return err
		}
		return nil // Already installed and functional
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
				// Already installed, verify CLI is functional
				if err := platform.CheckTailscaleCLIEnabled(); err != nil {
					return err
				}
				return nil
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
	if err := cmd.Run(); err != nil {
		return err
	}

	// On macOS, verify CLI is functional after installation
	// (App Store installations require manual CLI enabling in Settings)
	if platform.IsDarwin() {
		if err := platform.CheckTailscaleCLIEnabled(); err != nil {
			return err
		}
	}

	return nil
}

// joinTailscaleNetwork connects to the Tailscale network with the given credentials
func joinTailscaleNetwork(nodeName, authkey string) error {
	tailscaleLogout()

	tsCmd := tailscaleUpCommand(nodeName, authkey)
	output, err := tsCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tailscale up failed: %s", string(output))
	}
	return nil
}

// tailscaleLogout logs out of Tailscale (ignoring errors)
func tailscaleLogout() {
	tsPath := getTailscalePath()
	if tsPath == "" {
		return // Tailscale not installed, nothing to logout
	}
	if platform.IsWindows() {
		exec.Command(tsPath, "logout").Run()
	} else {
		exec.Command("sudo", tsPath, "logout").Run()
	}
}

// tailscaleUpCommand builds the tailscale up command for the platform
func tailscaleUpCommand(nodeName, authkey string) *exec.Cmd {
	tsPath := getTailscalePath()
	if tsPath == "" {
		tsPath = "tailscale" // Fallback to PATH lookup
	}
	if platform.IsWindows() {
		return exec.Command(tsPath, "up",
			"--login-server="+nexusURL,
			"--authkey="+authkey,
			"--hostname="+nodeName,
			"--accept-routes",
			"--accept-dns",
		)
	}
	return exec.Command("sudo", tsPath, "up",
		"--login-server="+nexusURL,
		"--authkey="+authkey,
		"--hostname="+nodeName,
		"--accept-routes",
		"--accept-dns",
	)
}

func init() {
	rootCmd.AddCommand(loginCmd)
	loginCmd.Flags().StringVar(&loginAuthkey, "authkey", "", "Pre-generated authkey for non-interactive login")
	loginCmd.Flags().StringVar(&loginNodeName, "node-name", "", "Override the node name (defaults to hostname)")
}
