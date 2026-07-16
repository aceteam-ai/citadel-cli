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

var (
	removeContainer bool
	forceStop       bool
	dryRunStop      bool
)

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

  # Stop and remove the container and volumes
  citadel stop ollama --rm

  # Skip confirmation prompts (for scripts)
  citadel stop --force

  # Preview what would be stopped
  citadel stop --dry-run`,
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

// confirmPrompt asks the user a yes/no question. Returns true if confirmed.
// defaultYes controls the default when the user presses Enter without typing.
// Skips the prompt and returns true if --force is set.
func confirmPrompt(question string, defaultYes bool) bool {
	if forceStop {
		return true
	}

	hint := "(y/N)"
	if defaultYes {
		hint = "(Y/n)"
	}
	fmt.Printf("%s %s ", question, hint)

	var response string
	fmt.Scanln(&response)
	response = strings.TrimSpace(strings.ToLower(response))

	if response == "" {
		return defaultYes
	}
	return response == "y" || response == "yes"
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

	// List services that will be affected
	serviceNames := make([]string, len(manifest.Services))
	for i, s := range manifest.Services {
		serviceNames[i] = s.Name
	}

	if dryRunStop {
		fmt.Printf("Would stop %d service(s): %s\n", len(manifest.Services), strings.Join(serviceNames, ", "))
		if removeContainer {
			fmt.Println("Would also remove containers and volumes.")
		}
		return
	}

	// Confirm before stopping all services
	if removeContainer {
		if !confirmPrompt(fmt.Sprintf("Remove %d service(s) and their volumes (%s)?", len(manifest.Services), strings.Join(serviceNames, ", ")), false) {
			fmt.Println("Aborted.")
			return
		}
	} else {
		if !confirmPrompt(fmt.Sprintf("Stop %d service(s) (%s)?", len(manifest.Services), strings.Join(serviceNames, ", ")), true) {
			fmt.Println("Aborted.")
			return
		}
	}

	fmt.Printf("--- 🛑 Stopping %d service(s) ---\n", len(manifest.Services))

	// Process in reverse order for graceful shutdown
	for i := len(manifest.Services) - 1; i >= 0; i-- {
		service := manifest.Services[i]
		fullComposePath := filepath.Join(configDir, service.ComposeFile)
		fmt.Printf("🔻 Stopping service: %s\n", service.Name)

		// Mark durably stopped FIRST (mirrors liveModuleOps.Stop, #528): the stop
		// must survive a `citadel work` restart / reboot, whose boot paths skip
		// services with desired_status: stopped. `citadel run [service]` clears
		// the marker again.
		if err := setServiceDesiredStatus(configDir, service.Name, "stopped"); err != nil {
			fmt.Fprintf(os.Stderr, "   ⚠️  Could not record stopped state for %s: %v\n", service.Name, err)
		}

		if err := stopServiceByCompose(fullComposePath, removeContainer); err != nil {
			fmt.Fprintf(os.Stderr, "   ❌ Failed to stop service %s: %v\n", service.Name, err)
		} else {
			fmt.Printf("   ✅ Service %s is stopped.\n", service.Name)
		}
		// Transitional (#528): also remove containers a pre-fix start left under
		// the legacy "citadel-<name>" compose project, invisible to the no-`-p`
		// down above.
		removeLegacyCitadelProject(service.Name)
	}

	fmt.Println("\n🎉 All services stopped.")
	fmt.Println("   Services stay stopped across restarts. Use 'citadel run <service>' to start one again.")
}

// stopSingleService stops a specific service.
func stopSingleService(serviceName string) {
	// Validate service name
	if _, ok := services.ServiceMap[serviceName]; !ok {
		fmt.Fprintf(os.Stderr, "❌ Unknown service '%s'.\n", serviceName)
		fmt.Printf("Available services: %s\n", strings.Join(services.GetAvailableServices(), ", "))
		os.Exit(1)
	}

	if dryRunStop {
		fmt.Printf("Would stop service: %s\n", serviceName)
		if removeContainer {
			fmt.Println("Would also remove container and volumes.")
		}
		return
	}

	// Confirm before removing volumes (data-destructive)
	if removeContainer {
		if !confirmPrompt(fmt.Sprintf("Remove service '%s' and its volumes?", serviceName), false) {
			fmt.Println("Aborted.")
			return
		}
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
		// Mark durably stopped FIRST (mirrors liveModuleOps.Stop, #528): the stop
		// must survive a `citadel work` restart / reboot, whose boot paths skip
		// services with desired_status: stopped. `citadel run <service>` clears
		// the marker again.
		if err := setServiceDesiredStatus(configDir, serviceName, "stopped"); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not record stopped state for %s: %v\n", serviceName, err)
		}
		// Use docker compose down if we have the compose file
		if err := stopServiceByCompose(composePath, removeContainer); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to stop service '%s': %v\n", serviceName, err)
			os.Exit(1)
		}
		// Transitional (#528): also remove containers a pre-fix start left under
		// the legacy "citadel-<name>" compose project, invisible to the no-`-p`
		// down above.
		removeLegacyCitadelProject(serviceName)
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
	// Inject CITADEL_WORKSPACE + host-port vars so compose files guarded with
	// ${VAR:?...} (transcribe/meeting workspace mount, #525) interpolate.
	cmd.Env = composeEnv()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose down failed:\n%s\n   Hint: Is Docker running? Check with 'docker info'", strings.TrimSpace(string(output)))
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
		return fmt.Errorf("container '%s' not found. Run 'citadel status' to see running services", containerName)
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
	stopCmd.Flags().BoolVarP(&forceStop, "force", "f", false, "Skip confirmation prompts.")
	stopCmd.Flags().BoolVar(&dryRunStop, "dry-run", false, "Show what would be stopped without doing it.")
}
