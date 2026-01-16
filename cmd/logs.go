// cmd/logs.go
/*
Copyright © 2025 Jason Sun <jason@aceteam.ai>
*/
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

var follow bool
var tail string

// logsCmd represents the logs command
var logsCmd = &cobra.Command{
	Use:   "logs <service-name>",
	Short: "Fetch the logs of a running service",
	Long: fmt.Sprintf(`Streams the logs for a service started with 'citadel run' or defined in citadel.yaml.
Available services: %s`, strings.Join(services.GetAvailableServices(), ", ")),
	Example: `  # View last 100 lines of vllm logs
  citadel logs vllm

  # Follow ollama logs in real-time
  citadel logs ollama -f

  # View last 50 lines and follow
  citadel logs llamacpp -f -t 50`,
	Args: cobra.ExactArgs(1), // Requires exactly one argument: the service name
	Run: func(cmd *cobra.Command, args []string) {
		serviceName := args[0]

		// First, try to find service in manifest
		var fullComposePath string
		manifest, configDir, err := findAndReadManifest()
		if err == nil {
			for _, s := range manifest.Services {
				if s.Name == serviceName {
					fullComposePath = filepath.Join(configDir, s.ComposeFile)
					break
				}
			}
		}

		// If found in manifest, use docker compose logs
		if fullComposePath != "" {
			dockerArgs := []string{"compose", "-f", fullComposePath, "logs"}
			if follow {
				dockerArgs = append(dockerArgs, "--follow")
			}
			if tail != "" {
				dockerArgs = append(dockerArgs, "--tail", tail)
			}

			logCmd := exec.Command("docker", dockerArgs...)
			logCmd.Stdout = os.Stdout
			logCmd.Stderr = os.Stderr

			fmt.Printf("--- Streaming logs for service '%s' (Ctrl+C to stop) ---\n", serviceName)
			if err := logCmd.Run(); err != nil {
				if exitError, ok := err.(*exec.ExitError); ok {
					if exitError.ExitCode() != 130 {
						fmt.Fprintf(os.Stderr, "  ❌ Error: %v\n", err)
					}
				} else {
					fmt.Fprintf(os.Stderr, "  ❌ Error executing docker logs: %v\n", err)
				}
			}
			return
		}

		// Fallback: try direct container access (for 'citadel run' services)
		containerName := fmt.Sprintf("citadel-%s", serviceName)

		// Check if container exists
		inspectCmd := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", containerName)
		if _, err := inspectCmd.Output(); err != nil {
			// Validate service name before giving error
			if _, ok := services.ServiceMap[serviceName]; !ok {
				fmt.Fprintf(os.Stderr, "❌ Unknown service '%s'.\n", serviceName)
				fmt.Printf("Available services: %s\n", strings.Join(services.GetAvailableServices(), ", "))
			} else {
				fmt.Fprintf(os.Stderr, "❌ Container '%s' not found. Is the service running?\n", containerName)
			}
			os.Exit(1)
		}

		// Use docker logs directly
		dockerArgs := []string{"logs"}
		if follow {
			dockerArgs = append(dockerArgs, "-f")
		}
		if tail != "" {
			dockerArgs = append(dockerArgs, "--tail", tail)
		}
		dockerArgs = append(dockerArgs, containerName)

		logCmd := exec.Command("docker", dockerArgs...)
		logCmd.Stdout = os.Stdout
		logCmd.Stderr = os.Stderr

		fmt.Printf("--- Streaming logs for service '%s' (Ctrl+C to stop) ---\n", serviceName)
		if err := logCmd.Run(); err != nil {
			if exitError, ok := err.(*exec.ExitError); ok {
				if exitError.ExitCode() != 130 {
					fmt.Fprintf(os.Stderr, "  ❌ Error: %v\n", err)
				}
			} else {
				fmt.Fprintf(os.Stderr, "  ❌ Error executing docker logs: %v\n", err)
			}
		}
	},
}

// NOTE: The init() function remains the same.
func init() {
	rootCmd.AddCommand(logsCmd)

	// Add flags for 'follow' and 'tail' to mimic 'docker logs'
	logsCmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output.")
	logsCmd.Flags().StringVarP(&tail, "tail", "t", "100", "Number of lines to show from the end of the logs.")
}
