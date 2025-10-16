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
		if err := waitForTailscaleDaemon(); err != nil {
			return err
		}
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
			fmt.Println("✅ Tailscale login verified.")
		}

		if err := prepareCacheDirectories(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error preparing cache: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("--- Launching services ---")
		for _, service := range manifest.Services {
			fmt.Printf("🚀 Starting service: %s (%s)\n", service.Name, service.ComposeFile)
			err := startService(service)
			if err != nil {
				// *** CHANGE: If any service fails, print the error and exit immediately. ***
				fmt.Fprintf(os.Stderr, "   ❌ Failed to start service %s: %v\n", service.Name, err)
				os.Exit(1)
			}
			fmt.Printf("   ✅ Service %s is up.\n", service.Name)
		}

		fmt.Println("\n🎉 Citadel Node is online and services are running.")

		// Start the agent as the final step
		fmt.Println("--- 🚀 Starting Citadel Agent ---")
		agentCmd.Run(cmd, args)
	},
}

// (waitForTailscaleDaemon, joinNetwork, checkTailscaleState, readManifest, startService functions remain the same)
func waitForTailscaleDaemon() error {
	fmt.Println("--- Waiting for Tailscale daemon to be ready...")
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
		// 0755 permissions are rwx for user, r-x for group/others.
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create cache directory %s: %w", dir, err)
		}
	}

	fmt.Println("✅ Cache directories are ready.")
	return nil
}

func joinNetwork(hostname, serverURL, key string) error {
	fmt.Printf("   - Running tailscale up with sudo...\n")
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

func checkTailscaleState() error {
	cmd := exec.Command("tailscale", "status")
	output, err := cmd.CombinedOutput()
	outputStr := string(output)
	if err != nil && !strings.Contains(outputStr, "Logged out") {
		return fmt.Errorf("tailscale daemon is not responding: %s", outputStr)
	}
	if authKey == "" {
		if strings.Contains(outputStr, "Logged out") {
			return fmt.Errorf("you are not logged into Tailscale. Please run 'citadel login' or use an --authkey")
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
	var manifestWrapper struct {
		Node     CitadelManifest `yaml:"node"`
		Services []Service       `yaml:"services"`
	}
	err = yaml.Unmarshal(data, &manifestWrapper)
	if err != nil {
		return nil, fmt.Errorf("could not parse YAML in %s: %w", filePath, err)
	}
	manifest.Name = manifestWrapper.Node.Name
	manifest.Tags = manifestWrapper.Node.Tags
	manifest.Services = manifestWrapper.Services
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
