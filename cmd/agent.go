// cmd/agent.go
package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aceboss/citadel-cli/internal/jobs"
	"github.com/aceboss/citadel-cli/internal/nexus"
	"github.com/aceboss/citadel-cli/internal/ui"
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
		status := ui.NewStatusLine()

		if cmd.CalledAs() == "agent" {
			fmt.Println()
			status.Info("Starting Citadel Agent")
		}

		client := nexus.NewClient(nexusURL)
		status.Thinking(fmt.Sprintf("Connecting to Nexus at %s", nexusURL))

		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		status.Success("Agent started - polling for jobs")
		fmt.Println()

	agentLoop:
		for {
			select {
			case <-ticker.C:
				job, err := client.GetNextJob()
				if err != nil {
					status.Warning(fmt.Sprintf("Error fetching job: %v", err))
					continue
				}
				if job != nil {
					executeJob(client, job, status)
				}
			case <-sigs:
				break agentLoop
			}
		}

		fmt.Println()
		status.Info("Shutting down agent")
		status.Success("Agent stopped")
	},
}

// executeJob is now a clean dispatcher. It finds the right handler and runs it.
func executeJob(client *nexus.Client, job *nexus.Job, statusLine *ui.StatusLine) (string, error) {
	var output []byte
	var err error
	var jobStatus string

	// Show job received
	statusLine.Working(fmt.Sprintf("Received job %s (%s)", job.ID[:8], job.Type))

	// Execute with spinner
	spinner := ui.NewSpinner(ui.StyleWorking)
	spinner.Start(fmt.Sprintf("Executing %s", job.Type))

	handler, ok := jobHandlers[job.Type]
	if !ok {
		err = fmt.Errorf("unsupported job type: %s", job.Type)
	} else {
		jobCtx := jobs.JobContext{}
		output, err = handler.Execute(jobCtx, job)
	}

	if err != nil {
		jobStatus = "FAILURE"
		errorMsg := fmt.Sprintf("Execution Error: %v", err)
		// Combine the error and any command output for a full report
		if len(output) > 0 {
			errorMsg = fmt.Sprintf("%s\n---\n%s", errorMsg, string(output))
		}
		output = []byte(errorMsg)
		spinner.Fail(fmt.Sprintf("Job %s failed: %v", job.ID[:8], err))
	} else {
		jobStatus = "SUCCESS"
		spinner.Success(fmt.Sprintf("Job %s completed", job.ID[:8]))
	}

	// Report status back to Nexus
	update := nexus.JobStatusUpdate{
		Status: jobStatus,
		Output: string(output),
	}

	if reportErr := client.UpdateJobStatus(job.ID, update); reportErr != nil {
		statusLine.Warning(fmt.Sprintf("Failed to report job %s status to Nexus: %v", job.ID[:8], reportErr))
	}

	return jobStatus, err
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
