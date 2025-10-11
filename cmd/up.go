package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Define Go structs that match the structure of our citadel.yaml file.
// The `yaml:"..."` tags tell the parser how to map the YAML keys to the struct fields.
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
	Short: "Brings a Citadel Node online and starts its services",
	Long: `Reads the citadel.yaml manifest in the current directory, registers
the node with the Nexus, establishes a secure tunnel, and launches the
defined services (e.g., via Docker Compose).`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("--- Bringing Citadel Node online ---")

		// 1. Read and parse the citadel.yaml manifest
		manifest, err := readManifest("citadel.yaml")
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error reading manifest: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Manifest loaded for node: %s\n", manifest.Name)

		// 2. (TODO) Register with Nexus and establish tunnel
		// For the MVP, we will just print what we would do.
		// The actual implementation would involve getting an auth key from the Nexus API.
		fmt.Println("   - (Simulating) Registering with Nexus...")
		fmt.Println("   - (Simulating) Establishing secure tunnel...")

		// 3. Launch all defined services
		fmt.Println("--- Launching services ---")
		for _, service := range manifest.Services {
			fmt.Printf("🚀 Starting service: %s (%s)\n", service.Name, service.ComposeFile)
			err := startService(service)
			if err != nil {
				fmt.Fprintf(os.Stderr, "   ❌ Failed to start service %s: %v\n", service.Name, err)
				// We could choose to continue or exit on failure. For now, we'll continue.
			} else {
				fmt.Printf("   ✅ Service %s is up.\n", service.Name)
			}
		}

		fmt.Println("\n🎉 Citadel Node is online and services are running.")
		// 4. (TODO) Start the background agent to listen for jobs
		fmt.Println("   - (Simulating) Starting background agent to listen for jobs...")
	},
}

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

	// Construct the docker-compose command
	composeCmd := exec.Command("docker-compose", "-f", s.ComposeFile, "up", "-d")
	
	// Capture output for better error messages
	output, err := composeCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker-compose failed: %s", string(output))
	}

	return nil
}

func init() {
	rootCmd.AddCommand(upCmd)
}

