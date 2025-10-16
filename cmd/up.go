// cmd/up.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// (Struct definitions remain the same)
type Service struct {
	Name        string   `yaml:"name"`
	Type        string   `yaml:"type"`
	Tags        []string `yaml:"tags"`
	Endpoint    string   `yaml:"endpoint"`
	ComposeFile string   `yaml:"compose_file"`
}

type CitadelManifest struct {
	Name     string    `yaml:"name"`
	Tags     []string  `yaml:"tags"`
	Services []Service `yaml:"services"`
}

// upCmd represents the up command
var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Brings a Citadel Node online and starts its services from a manifest",
	Long: `Reads the citadel.yaml manifest, joins the network, and launches services.
In interactive mode, it checks for an existing login.
In automated mode (with --authkey), it joins the network non-interactively.`,

	PreRunE: func(cmd *cobra.Command, args []string) error {
		// First, wait for the daemon to be responsive. This is crucial
		// after a fresh install or service start.
		if err := waitForTailscaleDaemon(); err != nil {
			return err
		}

		// Now, check the login status. This function handles both
		// interactive and authkey-based scenarios correctly.
		fmt.Println("--- Verifying Tailscale status...")
		return checkTailscaleState()
	},

	Run: func(cmd *cobra.Command, args []string) {
		manifest, err := readManifest("citadel.yaml")
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error reading manifest: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Manifest loaded for node: %s\n", manifest.Name)

		if authKey != "" {
			fmt.Println("--- Establishing secure tunnel via authkey ---")
			err = joinNetwork(manifest.Name, nexusURL, authKey)
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Error joining network: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("✅ Secure tunnel established.")
		} else {
			// The PreRunE check has already confirmed we are logged in.
			fmt.Println("✅ Tailscale login verified.")
		}

		fmt.Println("--- Launching services ---")
		for _, service := range manifest.Services {
			fmt.Printf("🚀 Starting service: %s (%s)\n", service.Name, service.ComposeFile)
			err := startService(service)
			if err != nil {
				fmt.Fprintf(os.Stderr, "   ❌ Failed to start service %s: %v\n", service.Name, err)
			} else {
				fmt.Printf("   ✅ Service %s is up.\n", service.Name)
			}
		}
		fmt.Println("\n🎉 Citadel Node is online and services are running.")
		fmt.Println("   - (TODO) Starting background agent to listen for jobs...")
	},
}

func waitForTailscaleDaemon() error {
	fmt.Println("--- Waiting for Tailscale daemon to be ready...")
	maxAttempts := 10
	for i := 0; i < maxAttempts; i++ {
		// Use a lightweight command like `tailscale version` which still needs the daemon
		cmd := exec.Command("tailscale", "version")
		if err := cmd.Run(); err == nil {
			fmt.Println("✅ Daemon is ready.")
			return nil // Success!
		}
		time.Sleep(500 * time.Millisecond) // Wait before retrying
	}
	return fmt.Errorf("timed out waiting for tailscaled daemon to start")
}

func joinNetwork(hostname, serverURL, key string) error {
	fmt.Printf("   - Running tailscale up with sudo...\n")

	// Clean slate, also with sudo
	exec.Command("sudo", "tailscale", "logout").Run()

	tsCmd := exec.Command("sudo", "tailscale", "up",
		"--login-server="+serverURL,
		"--authkey="+key,
		"--hostname="+hostname,
		"--accept-routes",
	)
	output, err := tsCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tailscale up failed: %s", string(output))
	}
	if !strings.Contains(string(output), "Success") {
		fmt.Printf("   - Tailscale output: %s\n", string(output))
	}
	return nil
}

// checkTailscaleState ensures the tailscale daemon is running. If no authkey
// is provided (interactive mode), it also ensures the user is logged in.
func checkTailscaleState() error {
	cmd := exec.Command("tailscale", "status")
	output, err := cmd.CombinedOutput()
	outputStr := string(output)

	// A non-nil error is OK only if the reason is that the user is logged out.
	// Any other error (e.g., "Failed to connect to tailscaled") is a real problem.
	if err != nil && !strings.Contains(outputStr, "Logged out") {
		return fmt.Errorf("tailscale daemon is not responding: %s", outputStr)
	}

	// Now, handle the interactive mode case. If no authkey is given,
	// the user MUST already be logged in.
	if authKey == "" {
		if strings.Contains(outputStr, "Logged out") {
			return fmt.Errorf("you are not logged into Tailscale. Please run 'citadel login' or use an --authkey")
		}
	}

	// If we're here, the state is valid:
	// 1. The daemon is running.
	// 2. EITHER we are using an authkey (so being logged out is fine)
	// 3. OR we are in interactive mode and have confirmed we are logged in.
	return nil
}

// (readManifest and startService functions are the same)
func readManifest(filePath string) (*CitadelManifest, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("could not read file %s: %w", filePath, err)
	}
	var manifest CitadelManifest
	err = yaml.Unmarshal(data, &manifest)
	if err != nil {
		return nil, fmt.Errorf("could not parse YAML in %s: %w", filePath, err)
	}
	return &manifest, nil
}

func startService(s Service) error {
	if s.ComposeFile == "" {
		return fmt.Errorf("service %s has no compose_file defined", s.Name)
	}

	composeCmd := exec.Command("docker", "compose", "-f", s.ComposeFile, "up", "-d")
	output, err := composeCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose failed: %s", string(output))
	}
	return nil
}

func init() {
	rootCmd.AddCommand(upCmd)
	upCmd.Flags().StringVar(&authKey, "authkey", "", "The pre-authenticated key to join the network (for automation)")
}
