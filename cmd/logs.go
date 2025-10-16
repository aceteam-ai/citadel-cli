// cmd/logs.go
/*
Copyright © 2025 Jason Sun <jason@aceteam.ai>
*/
package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

var follow bool
var tail string

// logsCmd represents the logs command
var logsCmd = &cobra.Command{
	Use:   "logs <service-name>",
	Short: "Fetch the logs of a running service",
	Long: `Streams the logs for a specified service defined in the citadel.yaml manifest.
This command uses 'docker compose logs' to retrieve the output from the service's containers.`,
	Args: cobra.ExactArgs(1), // Requires exactly one argument: the service name
	Run: func(cmd *cobra.Command, args []string) {
		serviceName := args[0]

		manifest, err := readManifest("citadel.yaml")
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Println("  🤷 No citadel.yaml found, cannot find service.")
				return
			}
			fmt.Fprintf(os.Stderr, "  ❌ Error reading manifest: %v\n", err)
			return
		}

		var targetService *Service
		for i, s := range manifest.Services {
			if s.Name == serviceName {
				targetService = &manifest.Services[i]
				break
			}
		}

		if targetService == nil {
			fmt.Fprintf(os.Stderr, "Service '%s' not found in citadel.yaml\n", serviceName)
			os.Exit(1)
		}

		dockerArgs := []string{
			"compose",
			"-f",
			targetService.ComposeFile,
			"logs",
		}

		if follow {
			dockerArgs = append(dockerArgs, "--follow")
		}
		if tail != "" {
			dockerArgs = append(dockerArgs, "--tail", tail)
		}

		logCmd := exec.Command("docker", dockerArgs...)

		// Pipe the command's output directly to the user's terminal
		logCmd.Stdout = os.Stdout
		logCmd.Stderr = os.Stderr

		fmt.Printf("--- Streaming logs for service '%s' (Ctrl+C to stop) ---\n", serviceName)
		if err := logCmd.Run(); err != nil {
			// The error is often just that the user pressed Ctrl+C, so we don't always need to print it.
			if exitError, ok := err.(*exec.ExitError); ok {
				if exitError.ExitCode() != 130 { // 130 = script terminated by Ctrl+C
					fmt.Fprintf(os.Stderr, "  ❌ Script terminated by Ctrl+C: %v\n", err)
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
