// cmd/run.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aceboss/citadel-cli/internal/compose"
	"github.com/aceboss/citadel-cli/internal/platform"
	"github.com/aceboss/citadel-cli/services"

	"github.com/spf13/cobra"
)

var detachRun bool

// runCmd represents the run command
var runCmd = &cobra.Command{
	Use:   "run [service]",
	Short: "Run a pre-packaged service like ollama, vllm, etc.",
	Long: fmt.Sprintf(`Deploys a containerized, pre-configured service onto the node.
This command is for running ad-hoc services and does not use the citadel.yaml manifest.
Available services: %s`, strings.Join(services.GetAvailableServices(), ", ")),
	Example: `  # Run vLLM in detached mode (background)
  citadel run vllm

  # Run Ollama without detaching (foreground)
  citadel run ollama --detach=false`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		serviceName := args[0]
		composeContent, ok := services.ServiceMap[serviceName]
		if !ok {
			fmt.Fprintf(os.Stderr, "❌ Unknown service '%s'.\n", serviceName)
			fmt.Printf("Available services: %s\n", strings.Join(services.GetAvailableServices(), ", "))
			os.Exit(1)
		}

		// Strip GPU device reservations on non-Linux platforms
		if !platform.IsLinux() {
			filtered, err := compose.StripGPUDevices([]byte(composeContent))
			if err == nil {
				composeContent = string(filtered)
				fmt.Println("   ℹ️  Running in CPU-only mode (GPU acceleration unavailable on this platform)")
			}
		}

		// Write the embedded content to a temporary file
		tmpDir := os.TempDir()
		tmpFileName := fmt.Sprintf("citadel-run-%s-compose.yml", serviceName)
		tmpFilePath := filepath.Join(tmpDir, tmpFileName)

		err := os.WriteFile(tmpFilePath, []byte(composeContent), 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to write temporary compose file: %v\n", err)
			os.Exit(1)
		}
		defer os.Remove(tmpFilePath) // Clean up after we're done

		fmt.Printf("--- 🚀 Launching pre-packaged service: %s ---\n", serviceName)

		// Use a unique project name to avoid conflicts
		projectName := fmt.Sprintf("citadel-run-%s", serviceName)
		composeArgs := []string{"compose", "-p", projectName, "-f", tmpFilePath, "up"}
		if detachRun {
			composeArgs = append(composeArgs, "-d")
		}

		runCmd := exec.Command("docker", composeArgs...)
		runCmd.Stdout = os.Stdout
		runCmd.Stderr = os.Stderr

		if err := runCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "\n❌ Failed to start service '%s'.\n", serviceName)
			os.Exit(1)
		}

		if detachRun {
			fmt.Printf("\n✅ Service '%s' is running in the background.\n", serviceName)
			// Updated help text to be more specific
			containerName := fmt.Sprintf("citadel-%s", serviceName)
			fmt.Printf("   - To see logs, run: docker logs %s -f\n", containerName)
			fmt.Printf("   - To stop, run: docker stop %s\n", containerName)
		}
	},
}

func init() {
	rootCmd.AddCommand(runCmd)
	runCmd.Flags().BoolVarP(&detachRun, "detach", "d", true, "Run in detached mode (background).")
}
