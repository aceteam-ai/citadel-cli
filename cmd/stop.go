// cmd/stop.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/spf13/cobra"
)

var removeContainer bool

// stopCmd represents the stop command
var stopCmd = &cobra.Command{
	Use:   "stop [service]",
	Short: "Stop services (all if no service specified, or a specific one)",
	Long: fmt.Sprintf(`Stops running services.

When a service name is provided, that specific service is stopped.
When no service is specified, all services in the manifest are stopped.

Available services: %s`, strings.Join(services.GetAvailableServices(), ", ")),
	Example: `  # Stop a specific service
  citadel stop vllm

  # Stop all services in the manifest
  citadel stop

  # Stop and remove the container
  citadel stop ollama --rm`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) == 0 {
			// No service specified - stop all manifest services
			stopAllServices()
			return
		}

		// Stop specific service
		serviceName := args[0]
		stopSingleService(serviceName)
	},
}

// stopAllServices stops all services defined in the manifest.
func stopAllServices() {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}

	if len(manifest.Services) == 0 {
		fmt.Println("No services configured in manifest.")
		return
	}

	fmt.Printf("--- 🛑 Stopping %d service(s) ---\n", len(manifest.Services))

	// Process in reverse order for graceful shutdown
	for i := len(manifest.Services) - 1; i >= 0; i-- {
		service := manifest.Services[i]
		fullComposePath := filepath.Join(configDir, service.ComposeFile)
		fmt.Printf("🔻 Stopping service: %s\n", service.Name)

		if err := stopServiceByCompose(fullComposePath, removeContainer); err != nil {
			fmt.Fprintf(os.Stderr, "   ❌ Failed to stop service %s: %v\n", service.Name, err)
		} else {
			fmt.Printf("   ✅ Service %s is stopped.\n", service.Name)
		}
	}

	fmt.Println("\n🎉 All services stopped.")
}

// stopSingleService stops a specific service.
func stopSingleService(serviceName string) {
	// Validate service name
	if _, ok := services.ServiceMap[serviceName]; !ok {
		fmt.Fprintf(os.Stderr, "❌ Unknown service '%s'.\n", serviceName)
		fmt.Printf("Available services: %s\n", strings.Join(services.GetAvailableServices(), ", "))
		os.Exit(1)
	}

	// Try to find the service in the manifest
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		// If no manifest, try to stop by container name directly
		fmt.Printf("--- 🛑 Stopping service: %s ---\n", serviceName)
		if err := stopServiceByContainer(serviceName); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Service '%s' stopped.\n", serviceName)
		return
	}

	// Find service in manifest
	var composePath string
	for _, s := range manifest.Services {
		if s.Name == serviceName {
			composePath = filepath.Join(configDir, s.ComposeFile)
			break
		}
	}

	fmt.Printf("--- 🛑 Stopping service: %s ---\n", serviceName)

	if composePath != "" {
		// Use docker compose down if we have the compose file
		if err := stopServiceByCompose(composePath, removeContainer); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to stop service '%s': %v\n", serviceName, err)
			os.Exit(1)
		}
	} else {
		// Fallback to direct container stop
		if err := stopServiceByContainer(serviceName); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Printf("✅ Service '%s' stopped.\n", serviceName)
}

// stopServiceByCompose stops a service using docker compose down.
func stopServiceByCompose(composePath string, remove bool) error {
	if _, err := os.Stat(composePath); os.IsNotExist(err) {
		return fmt.Errorf("compose file '%s' not found", composePath)
	}

	args := []string{"compose", "-f", composePath, "down"}
	if remove {
		args = append(args, "-v") // Also remove volumes
	}

	cmd := exec.Command("docker", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose down failed: %s", string(output))
	}
	return nil
}

// stopServiceByContainer stops a service by its container name directly.
func stopServiceByContainer(serviceName string) error {
	containerName := fmt.Sprintf("citadel-%s", serviceName)

	// Check if container exists
	inspectCmd := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", containerName)
	output, err := inspectCmd.Output()
	if err != nil {
		return fmt.Errorf("container '%s' not found. Is the service running?", containerName)
	}

	status := strings.TrimSpace(string(output))
	if status != "running" {
		fmt.Printf("   ℹ️  Container '%s' is not running (status: %s).\n", containerName, status)
		if removeContainer {
			return removeContainerByName(containerName)
		}
		return nil
	}

	// Stop the container
	stopCmd := exec.Command("docker", "stop", containerName)
	stopCmd.Stdout = os.Stdout
	stopCmd.Stderr = os.Stderr
	if err := stopCmd.Run(); err != nil {
		return fmt.Errorf("failed to stop container")
	}

	if removeContainer {
		return removeContainerByName(containerName)
	}
	return nil
}

// removeContainerByName removes a container by name.
func removeContainerByName(containerName string) error {
	fmt.Printf("--- Removing container '%s' ---\n", containerName)
	rmCmd := exec.Command("docker", "rm", containerName)
	rmCmd.Stdout = os.Stdout
	rmCmd.Stderr = os.Stderr
	if err := rmCmd.Run(); err != nil {
		return fmt.Errorf("failed to remove container")
	}
	fmt.Printf("✅ Container '%s' removed.\n", containerName)
	return nil
}

func init() {
	rootCmd.AddCommand(stopCmd)
	stopCmd.Flags().BoolVar(&removeContainer, "rm", false, "Remove the container/volumes after stopping.")
}
