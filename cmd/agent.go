package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aceboss/citadel-cli/internal/jobs"
	"github.com/aceboss/citadel-cli/internal/nexus"
	"github.com/spf13/cobra"
)

//  The agent acts as a dispatcher, delegating all the complex work to the appropriate handlers

// A map to hold all our registered job handlers.
var jobHandlers map[string]jobs.JobHandler

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run the Citadel agent to listen for jobs from the Nexus",
	Long: `This is a long-running command that connects to the AceTeam Nexus
and waits for remote jobs to execute on this node. It should typically be
run as a background service.`,
	Run: func(cmd *cobra.Command, args []string) {
		if cmd.CalledAs() == "agent" {
			fmt.Println("--- 🚀 Starting Citadel Agent ---")
		}
		client := nexus.NewClient(nexusURL)
		fmt.Printf("   - Nexus endpoint: %s\n", nexusURL)
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		fmt.Println("   - ✅ Agent started. Polling for jobs...")
	agentLoop:
		for {
			select {
			case <-ticker.C:
				job, err := client.GetNextJob()
				if err != nil {
					fmt.Fprintf(os.Stderr, "   - ⚠️ Error fetching job: %v\n", err)
					continue
				}
				if job != nil {
					fmt.Printf("   - 📥 Received job %s of type %s. Executing...\n", job.ID, job.Type)
					executeJob(client, job)
				}
			case <-sigs:
				break agentLoop
			}
		}
		fmt.Println("\n--- 🛑 Shutting down agent ---")
		fmt.Println("   - ✅ Agent stopped.")
	},
}

// executeJob is now a clean dispatcher. It finds the right handler and runs it.
func executeJob(client *nexus.Client, job *nexus.Job) (string, error) {
	var output []byte
	var err error
	var status string

	handler, ok := jobHandlers[job.Type]
	if !ok {
		err = fmt.Errorf("unsupported job type: %s", job.Type)
	} else {
		jobCtx := jobs.JobContext{}
		output, err = handler.Execute(jobCtx, job)
	}

	if err != nil {
		status = "FAILURE"
		errorMsg := fmt.Sprintf("Execution Error: %v", err)
		// Combine the error and any command output for a full report
		if len(output) > 0 {
			errorMsg = fmt.Sprintf("%s\n---\n%s", errorMsg, string(output))
		}
		output = []byte(errorMsg)
		fmt.Fprintf(os.Stderr, "     - [Job %s] ❌ Execution failed: %v\n", job.ID, err)
	} else {
		status = "SUCCESS"
		fmt.Printf("     - [Job %s] ✅ Execution successful.\n", job.ID)
	}

	update := nexus.JobStatusUpdate{
		Status: status,
		Output: string(output),
	}

	if reportErr := client.UpdateJobStatus(job.ID, update); reportErr != nil {
		fmt.Fprintf(os.Stderr, "     - [Job %s] ⚠️ CRITICAL: Failed to report status back to Nexus: %v\n", job.ID, reportErr)
	}
	return status, err
}

func init() {
	rootCmd.AddCommand(agentCmd)

	// Register all our job handlers. To add a new job type, you just add a line here.
	jobHandlers = map[string]jobs.JobHandler{
		"SHELL_COMMAND":      &jobs.ShellCommandHandler{},
		"DOWNLOAD_MODEL":     &jobs.DownloadModelHandler{},
		"OLLAMA_PULL":        &jobs.OllamaPullHandler{},
		"LLAMACPP_INFERENCE": &jobs.LlamaCppInferenceHandler{},
		"VLLM_INFERENCE":     &jobs.VLLMInferenceHandler{},
		"OLLAMA_INFERENCE":   &jobs.OllamaInferenceHandler{},
	}
}
