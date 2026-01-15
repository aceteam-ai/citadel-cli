// cmd/stop.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/aceteam-ai/citadel-cli/services"

	"github.com/spf13/cobra"
)

var removeContainer bool

// stopCmd represents the stop command
var stopCmd = &cobra.Command{
	Use:   "stop [service]",
	Short: "Stop a running service",
	Long: fmt.Sprintf(`Stops a containerized service that was started with 'citadel run'.
Available services: %s`, strings.Join(services.GetAvailableServices(), ", ")),
	Example: `  # Stop a running vLLM service
  citadel stop vllm

  # Stop and remove the container
  citadel stop ollama --rm`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		serviceName := args[0]

		// Validate service name
		if _, ok := services.ServiceMap[serviceName]; !ok {
			fmt.Fprintf(os.Stderr, "❌ Unknown service '%s'.\n", serviceName)
			fmt.Printf("Available services: %s\n", strings.Join(services.GetAvailableServices(), ", "))
			os.Exit(1)
		}

		containerName := fmt.Sprintf("citadel-%s", serviceName)

		// Check if container exists
		inspectCmd := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", containerName)
		output, err := inspectCmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Container '%s' not found. Is the service running?\n", containerName)
			os.Exit(1)
		}

		status := strings.TrimSpace(string(output))
		if status != "running" {
			fmt.Printf("   ℹ️  Container '%s' is not running (status: %s).\n", containerName, status)
			if removeContainer {
				fmt.Printf("--- Removing container '%s' ---\n", containerName)
				rmCmd := exec.Command("docker", "rm", containerName)
				rmCmd.Stdout = os.Stdout
				rmCmd.Stderr = os.Stderr
				if err := rmCmd.Run(); err != nil {
					fmt.Fprintf(os.Stderr, "❌ Failed to remove container.\n")
					os.Exit(1)
				}
				fmt.Printf("✅ Container '%s' removed.\n", containerName)
			}
			return
		}

		fmt.Printf("--- Stopping service: %s ---\n", serviceName)

		stopCmd := exec.Command("docker", "stop", containerName)
		stopCmd.Stdout = os.Stdout
		stopCmd.Stderr = os.Stderr

		if err := stopCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to stop service '%s'.\n", serviceName)
			os.Exit(1)
		}

		fmt.Printf("✅ Service '%s' stopped.\n", serviceName)

		if removeContainer {
			fmt.Printf("--- Removing container '%s' ---\n", containerName)
			rmCmd := exec.Command("docker", "rm", containerName)
			rmCmd.Stdout = os.Stdout
			rmCmd.Stderr = os.Stderr
			if err := rmCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "❌ Failed to remove container.\n")
				os.Exit(1)
			}
			fmt.Printf("✅ Container '%s' removed.\n", containerName)
		}
	},
}

func init() {
	rootCmd.AddCommand(stopCmd)
	stopCmd.Flags().BoolVar(&removeContainer, "rm", false, "Remove the container after stopping.")
}
