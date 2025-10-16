// cmd/agent.go
package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aceboss/citadel-cli/internal/nexus"
	"github.com/spf13/cobra"
)

// (agentCmd definition remains the same)
var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run the Citadel agent to listen for jobs from the Nexus",
	Long: `This is a long-running command that connects to the AceTeam Nexus
and waits for remote jobs to execute on this node. It should typically be
run as a background service.`,
	Run: func(cmd *cobra.Command, args []string) {
		// This check prevents the agent from starting twice if called from 'up'
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
					go executeJob(client, job)
				}
			case <-sigs:
				break agentLoop
			}
		}
		fmt.Println("\n--- 🛑 Shutting down agent ---")
		fmt.Println("   - ✅ Agent stopped.")
	},
}

type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type ollamaSuccessResponse struct {
	Response string `json:"response"`
}

type ollamaErrorResponse struct {
	Error string `json:"error"`
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
		parts := strings.Fields(cmdString)
		cmd := exec.Command(parts[0], parts[1:]...)
		output, err = cmd.CombinedOutput()

	case "OLLAMA_INFERENCE":
		model, modelOk := job.Payload["model"]
		prompt, promptOk := job.Payload["prompt"]
		if !modelOk || !promptOk {
			err = fmt.Errorf("job payload missing 'model' or 'prompt' field")
			break
		}

		fmt.Printf("     - [Job %s] Running Ollama inference on model '%s'\n", job.ID, model)

		ollamaURL := "http://localhost:11434/api/generate"
		reqPayload := ollamaRequest{
			Model:  model,
			Prompt: prompt,
			Stream: false,
		}
		reqBody, _ := json.Marshal(reqPayload)

		resp, httpErr := http.Post(ollamaURL, "application/json", bytes.NewBuffer(reqBody))
		if httpErr != nil {
			err = fmt.Errorf("failed to connect to ollama service: %w", httpErr)
			break
		}
		defer resp.Body.Close()

		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			err = fmt.Errorf("failed to read ollama response body: %w", readErr)
			break
		}

		// Check if Ollama returned a non-success status code (e.g., 404 for model not found)
		if resp.StatusCode != http.StatusOK {
			var ollamaErr ollamaErrorResponse
			if json.Unmarshal(respBody, &ollamaErr) == nil {
				// We successfully parsed the JSON error from Ollama
				err = fmt.Errorf("ollama API error: %s", ollamaErr.Error)
			} else {
				// We couldn't parse the error, so just return the raw response
				err = fmt.Errorf("ollama API returned status %d: %s", resp.StatusCode, string(respBody))
			}
			break
		}

		// If we're here, the request was successful. Parse the success response.
		var ollamaResp ollamaSuccessResponse
		if jsonErr := json.Unmarshal(respBody, &ollamaResp); jsonErr != nil {
			err = fmt.Errorf("failed to parse ollama success response: %w", jsonErr)
			break
		}
		output = []byte(ollamaResp.Response)

	default:
		err = fmt.Errorf("unsupported job type: %s", job.Type)
	}

	if err != nil {
		status = "FAILURE"
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
