// cmd/agent.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aceboss/citadel-cli/internal/nexus"
	"github.com/spf13/cobra"
)

// agentCmd represents the agent command
var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run the Citadel agent to listen for jobs from the Nexus",
	Long: `This is a long-running command that connects to the AceTeam Nexus
and waits for remote jobs to execute on this node. It should typically be
run as a background service.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("--- 🚀 Starting Citadel Agent ---")
		client := nexus.NewClient(nexusURL)
		fmt.Printf("   - Nexus endpoint: %s\n", nexusURL)

		// Create a channel to listen for OS signals for graceful shutdown
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

		// This is the main agent loop.
		ticker := time.NewTicker(5 * time.Second) // Poll for jobs every 5 seconds
		defer ticker.Stop()

		fmt.Println("   - ✅ Agent started. Polling for jobs...")

	agentLoop:
		for {
			select {
			case <-ticker.C:
				job, err := client.GetNextJob()
				if err != nil {
					fmt.Fprintf(os.Stderr, "   - ⚠️ Error fetching job: %v\n", err)
					continue // Don't stop, just try again on the next tick
				}

				if job != nil {
					fmt.Printf("   - 📥 Received job %s of type %s. Executing in background...\n", job.ID, job.Type)
					// Execute the job in a goroutine so we don't block the main loop.
					// The agent can immediately poll for another job.
					go executeJob(client, job)
				}
			case <-sigs:
				// Signal received, break the loop.
				break agentLoop
			}
		}

		fmt.Println("\n--- 🛑 Shutting down agent ---")
		fmt.Println("   - ✅ Agent stopped.")
	},
}

// executeJob runs the job, captures its output, and reports the status back to Nexus.
func executeJob(client *nexus.Client, job *nexus.Job) {
	var output []byte
	var err error
	var status string

	switch job.Type {
	case "SHELL_COMMAND":
		cmdString, ok := job.Payload["command"]
		if !ok {
			err = fmt.Errorf("job payload missing 'command' field")
			break
		}
		fmt.Printf("     - [Job %s] Running shell command: '%s'\n", job.ID, cmdString)
		// Use "sh -c" to properly handle commands with pipes, redirects, etc.
		parts := strings.Fields(cmdString)
		cmd := exec.Command(parts[0], parts[1:]...)
		output, err = cmd.CombinedOutput() // Captures both stdout and stderr
	default:
		err = fmt.Errorf("unsupported job type: %s", job.Type)
	}

	if err != nil {
		status = "FAILURE"
		// Prepend the error message to the output for better context
		output = []byte(fmt.Sprintf("Execution Error: %v\n---\n%s", err, string(output)))
		fmt.Fprintf(os.Stderr, "     - [Job %s] ❌ Execution failed: %v\n", job.ID, err)
	} else {
		status = "SUCCESS"
		fmt.Printf("     - [Job %s] ✅ Execution successful.\n", job.ID)
	}

	update := nexus.JobStatusUpdate{
		Status: status,
		Output: string(output),
	}

	if err := client.UpdateJobStatus(job.ID, update); err != nil {
		fmt.Fprintf(os.Stderr, "     - [Job %s] ⚠️ CRITICAL: Failed to report status back to Nexus: %v\n", job.ID, err)
	}
}

func init() {
	rootCmd.AddCommand(agentCmd)
}
