// cmd/job_handlers.go
// Job execution helpers used by test.go for diagnostic testing
package cmd

import (
	"fmt"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// A map to hold all our registered job handlers.
var jobHandlers map[string]jobs.JobHandler

// executeJob finds the right handler and runs a job.
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
	// Register all job handlers for test command
	jobHandlers = map[string]jobs.JobHandler{
		"SHELL_COMMAND":      &jobs.ShellCommandHandler{},
		"DOWNLOAD_MODEL":     &jobs.DownloadModelHandler{},
		"OLLAMA_PULL":        &jobs.OllamaPullHandler{},
		"LLAMACPP_INFERENCE": &jobs.LlamaCppInferenceHandler{},
		"VLLM_INFERENCE":     &jobs.VLLMInferenceHandler{},
		"OLLAMA_INFERENCE":   &jobs.OllamaInferenceHandler{},
	}
}
