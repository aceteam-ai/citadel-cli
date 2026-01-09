// cmd/up.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var servicesOnly bool

// upCmd represents the up command
var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Brings a Citadel Node online and starts its services from a manifest",
	Long: `Reads the citadel.yaml manifest, joins the network, and launches services.
In interactive mode, it checks for an existing login.
In automated mode (with --authkey), it joins the network non-interactively.`,
	Example: `  # Start services with existing network login
  citadel up

  # Start services with a new authkey (automated/CI)
  citadel up --authkey <your-key>`,

	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := waitForTailscaleDaemon(); err != nil {
			return err
		}
		fmt.Println("--- Verifying network status...")
		return checkTailscaleState()
	},

	Run: func(cmd *cobra.Command, args []string) {
		manifest, configDir, err := findAndReadManifest()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error loading configuration: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Manifest loaded for node: %s\n", manifest.Node.Name)

		if authkey != "" {
			fmt.Println("--- Establishing secure tunnel via authkey ---")
			err = joinNetwork(manifest.Node.Name, nexusURL, authkey)
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Error joining network: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("✅ Secure tunnel established.")
		} else {
			fmt.Println("✅ Network login verified.")
		}

		if err := prepareCacheDirectories(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error preparing cache: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("--- Launching services ---")
		for _, service := range manifest.Services {
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			fmt.Printf("🚀 Starting service: %s (%s)\n", service.Name, fullComposePath)
			err := startService(service.Name, fullComposePath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "   ❌ Failed to start service %s: %v\n", service.Name, err)
				os.Exit(1)
			}
			fmt.Printf("   ✅ Service %s is up.\n", service.Name)
		}

		fmt.Println("\n🎉 Citadel Node is online and services are running.")

		if servicesOnly {
			return // Exit before starting the agent
		}

		// Start the agent as the final step
		agentCmd.Run(cmd, args)
	},
}

func waitForTailscaleDaemon() error {
	fmt.Println("--- Waiting for Network daemon to be ready...")
	maxAttempts := 10
	for i := 0; i < maxAttempts; i++ {
		cmd := exec.Command("tailscale", "version")
		if err := cmd.Run(); err == nil {
			fmt.Println("✅ Daemon is ready.")
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for tailscaled daemon to start")
}

func prepareCacheDirectories() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("could not find user home directory: %w", err)
	}

	cacheBase := filepath.Join(homeDir, "citadel-cache")
	// A list of all potential cache directories our services might use.
	dirsToCreate := []string{
		filepath.Join(cacheBase, "ollama"),
		filepath.Join(cacheBase, "vllm"),
		filepath.Join(cacheBase, "llamacpp"),
		filepath.Join(cacheBase, "lmstudio"),
		filepath.Join(cacheBase, "huggingface"),
	}

	fmt.Println("--- Preparing cache directories ---")
	// First, create the base directory
	if err := os.MkdirAll(cacheBase, 0755); err != nil {
		return fmt.Errorf("failed to create base cache directory %s: %w", cacheBase, err)
	}

	// Then create all the subdirectories
	for _, dir := range dirsToCreate {
		// 0655 permissions are rwx for user, group, and others.
		// This solves the Docker volume permission issue for the container user.
		if err := os.MkdirAll(dir, 0655); err != nil {
			return fmt.Errorf("failed to create cache directory %s: %w", dir, err)
		}
	}

	fmt.Println("✅ Cache directories are ready.")
	return nil
}

func joinNetwork(hostname, serverURL, key string) error {
	fmt.Printf("   - Bringing network up with sudo...\n")
	exec.Command("sudo", "tailscale", "logout").Run()
	tsCmd := exec.Command("sudo", "tailscale", "up",
		"--login-server="+serverURL,
		"--authkey="+key,
		"--hostname="+hostname,
		"--accept-routes",
		"--accept-dns",
	)
	output, err := tsCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Network up failed: %s", string(output))
	}
	if !strings.Contains(string(output), "Success") {
		fmt.Printf("   - Network output: %s\n", string(output))
	}
	return nil
}

func checkTailscaleState() error {
	cmd := exec.Command("tailscale", "status")
	output, err := cmd.CombinedOutput()
	outputStr := string(output)
	if err != nil && !strings.Contains(outputStr, "Logged out") {
		return fmt.Errorf("tailscale daemon is not responding: %s", outputStr)
	}
	if authkey == "" {
		if strings.Contains(outputStr, "Logged out") {
			return fmt.Errorf("you are not logged into Network. Please run 'citadel login' or use an --authkey")
		}
	}
	return nil
}

func readManifest(filePath string) (*CitadelManifest, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("could not read file %s: %w", filePath, err)
	}
	var manifest CitadelManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("could not parse YAML in %s: %w", filePath, err)
	}
	return &manifest, nil
}

func startService(serviceName, composeFilePath string) error {
	if composeFilePath == "" {
		return fmt.Errorf("service %s has no compose_file defined", serviceName)
	}
	composeCmd := exec.Command("docker", "compose", "-f", composeFilePath, "up", "-d")
	output, err := composeCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose failed: %s", string(output))
	}
	return nil
}

func init() {
	rootCmd.AddCommand(upCmd)
	upCmd.Flags().StringVar(&authkey, "authkey", "", "The pre-authenticated key to join the network (for automation)")
	upCmd.Flags().BoolVar(&servicesOnly, "services-only", false, "Only start services and exit (internal use for init)")
	upCmd.Flags().MarkHidden("services-only")
}
