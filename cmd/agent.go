// cmd/agent.go
package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
					// go executeJob(client, job)
					// NOTE: the 'go' keyword to process jobs sequentially but can cause race condition ***
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

func getUserHomeDir() string {
	// When running under sudo, SUDO_USER has the original user's name
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		// This is a bit of a shortcut; a more robust way would be to look up the user.
		// For our case, this is reliable enough.
		return "/home/" + sudoUser
	}
	// Fallback for when not running under sudo
	homeDir, _ := os.UserHomeDir()
	return homeDir
}

// executeJob runs the job, captures its output, and reports the status back to Nexus.
func executeJob(client *nexus.Client, job *nexus.Job) (string, error) {
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

	case "DOWNLOAD_MODEL":
		repoURL, repoOk := job.Payload["repo_url"]
		fileName, fileOk := job.Payload["file_name"]
		modelType, typeOk := job.Payload["model_type"]
		if !repoOk || !fileOk || !typeOk {
			err = fmt.Errorf("job payload missing 'repo_url', 'file_name', or 'model_type'")
			break
		}

		fullURL, urlErr := url.JoinPath(repoURL, "resolve/main", fileName)
		if urlErr != nil {
			err = fmt.Errorf("could not construct valid download URL: %w", urlErr)
			break
		}

		// Use the current user's home directory, which is correct since the agent now runs as the user.
		homeDir, _ := os.UserHomeDir()
		destDir := filepath.Join(homeDir, "citadel-cache", modelType)
		destPath := filepath.Join(destDir, fileName)

		fmt.Printf("     - [Job %s] Preparing to download model to %s\n", job.ID, destPath)

		// This ensures that even if Docker created it first as root, we can still use it.
		// A better long-term fix is to not run the agent as root, but this is robust.
		// Let's create the parent directory first.
		parentDir := filepath.Dir(destDir)
		if err = os.MkdirAll(parentDir, 0777); err != nil {
			err = fmt.Errorf("failed to create parent cache directory %s: %w", parentDir, err)
			break
		}
		// Now create the final directory.
		if err = os.MkdirAll(destDir, 0777); err != nil {
			err = fmt.Errorf("failed to create destination directory %s: %w", destDir, err)
			break
		}

		// Let's also be explicit about setting permissions.
		exec.Command("chmod", "-R", "777", parentDir).Run()

		if _, statErr := os.Stat(destPath); statErr == nil {
			fmt.Printf("     - [Job %s] Model already exists. Skipping download.\n", job.ID)
			output = []byte(fmt.Sprintf("Model '%s' already exists at %s", fileName, destPath))
			break
		}

		// Use curl to download the file.
		fmt.Printf("     - [Job %s] Starting download...\n", job.ID)
		cmd := exec.Command("curl", "-L", "--create-dirs", "-o", destPath, fullURL)
		output, err = cmd.CombinedOutput()

	case "OLLAMA_PULL":
		model, modelOk := job.Payload["model"]
		if !modelOk {
			err = fmt.Errorf("job payload missing 'model' field")
			break
		}
		fmt.Printf("     - [Job %s] Pulling Ollama model '%s'\n", job.ID, model)
		// We execute the command inside the running container
		cmd := exec.Command("docker", "exec", "citadel-ollama", "ollama", "pull", model)
		output, err = cmd.CombinedOutput()

	case "LLAMACPP_INFERENCE":
		modelFile, modelOk := job.Payload["model_file"]
		prompt, promptOk := job.Payload["prompt"]
		if !modelOk || !promptOk {
			err = fmt.Errorf("job payload missing 'model_file' or 'prompt' field")
			break
		}

		// --- Step 1: Restart the llama.cpp container with the correct model ---
		fmt.Printf("     - [Job %s] Configuring llama.cpp to use model '%s'\n", job.ID, modelFile)

		// This is a simplification. A better way would be to read the manifest to find the compose file path.
		homeDir := getUserHomeDir()
		composeFile := filepath.Join(homeDir, "services/llamacpp.yml")

		// The new command for the container
		newCommand := fmt.Sprintf("--model /models/%s --host 0.0.0.0 --port 8080 --n-gpu-layers -1", modelFile)

		// Use `docker compose up`. It will recreate the container if its config (like the command) has changed.
		restartCmd := exec.Command("docker", "compose", "-f", composeFile, "up", "-d", "--force-recreate")

		// Set the environment variable to pass the new command to the container
		restartCmd.Env = append(os.Environ(), fmt.Sprintf("LLAMACPP_COMMAND=%s", newCommand))

		if output, restartErr := restartCmd.CombinedOutput(); restartErr != nil {
			err = fmt.Errorf("failed to restart llama.cpp service with new model: %s", string(output))
			break
		}

		// Give the server a moment to start up and load the model
		fmt.Printf("     - [Job %s] Waiting for llama.cpp server to initialize...\n", job.ID)

		llamacppURL := "http://localhost:8080/completion"
		ready := false
		maxWait := 120 * time.Second // Give it up to 120 seconds to load a large model
		pollInterval := 2 * time.Second
		startTime := time.Now()

		for time.Since(startTime) < maxWait {
			// We send a dummy POST request.
			resp, httpErr := http.Post(llamacppURL, "application/json", bytes.NewBuffer([]byte("{}")))

			if httpErr != nil {
				// This is a network error (e.g., connection refused). The server is not up yet.
				time.Sleep(pollInterval)
				continue // Try again
			}

			// The server is up, but is it ready? Check the status code.
			// The server helpfully returns 503 while it's loading the model.
			if resp.StatusCode == http.StatusServiceUnavailable {
				resp.Body.Close() // Always close the body
				time.Sleep(pollInterval)
				continue // Not ready yet, try again
			}

			// Any other response (200, 400, 500, etc.) means the model has finished loading
			// and the server is actively processing requests. We are ready.
			resp.Body.Close()
			ready = true
			break
		}

		if !ready {
			err = fmt.Errorf("llama.cpp server did not become ready within %v", maxWait)
			break
		}

		// --- Step 2: Perform the inference ---
		fmt.Printf("     - [Job %s] Running Llama.cpp inference\n", job.ID)

		requestPayload := map[string]interface{}{
			"prompt":      prompt,
			"n_predict":   256,
			"temperature": 0.7,
		}
		reqBody, _ := json.Marshal(requestPayload)

		resp, httpErr := http.Post(llamacppURL, "application/json", bytes.NewBuffer(reqBody))
		if httpErr != nil {
			err = fmt.Errorf("failed to connect to llama.cpp service: %w", httpErr)
			break
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			err = fmt.Errorf("llama.cpp API returned non-200 status: %s, Body: %s", resp.Status, string(bodyBytes))
			break
		}

		var responsePayload map[string]interface{}
		if json.NewDecoder(resp.Body).Decode(&responsePayload) != nil {
			err = fmt.Errorf("failed to parse llama.cpp response")
			break
		}
		content, ok := responsePayload["content"].(string)
		if !ok {
			err = fmt.Errorf("could not find 'content' in llama.cpp response: %+v", responsePayload)
			break
		}
		output = []byte(content)

	case "VLLM_INFERENCE":
		model, modelOk := job.Payload["model"]
		prompt, promptOk := job.Payload["prompt"]
		if !modelOk || !promptOk {
			err = fmt.Errorf("job payload missing 'model' or 'prompt' field")
			break
		}

		// --- HEALTH CHECK BLOCK ---
		fmt.Printf("     - [Job %s] Waiting for vLLM service to become ready...\n", job.ID)
		vllmHealthURL := "http://localhost:8000/health"
		ready := false
		maxWait := 60 * time.Second // Wait up to 60 seconds
		pollInterval := 1 * time.Second
		startTime := time.Now()

		for time.Since(startTime) < maxWait {
			resp, httpErr := http.Get(vllmHealthURL)
			if httpErr == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				ready = true
				break
			}
			if resp != nil {
				resp.Body.Close()
			}
			time.Sleep(pollInterval)
		}

		if !ready {
			err = fmt.Errorf("vllm service did not become ready within %v", maxWait)
			break
		}
		fmt.Printf("     - [Job %s] vLLM service is ready. Running inference on model '%s'\n", job.ID, model)
		// --- END OF HEALTH CHECK BLOCK ---

		vllmCompletionsURL := "http://localhost:8000/v1/completions"
		requestPayload := map[string]interface{}{
			"model":       model,
			"prompt":      prompt,
			"max_tokens":  512,
			"temperature": 0.7,
		}
		reqBody, _ := json.Marshal(requestPayload)

		resp, httpErr := http.Post(vllmCompletionsURL, "application/json", bytes.NewBuffer(reqBody))
		if httpErr != nil {
			err = fmt.Errorf("failed to connect to vllm service: %w", httpErr)
			break
		}
		defer resp.Body.Close()

		bodyBytes, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("vllm API returned non-200 status: %s, Body: %s", resp.Status, string(bodyBytes))
			break
		}

		var responsePayload struct {
			Choices []struct {
				Text string `json:"text"`
			} `json:"choices"`
		}
		if json.Unmarshal(bodyBytes, &responsePayload) != nil || len(responsePayload.Choices) == 0 {
			err = fmt.Errorf("failed to parse vLLM response or no choices returned. Raw: %s", string(bodyBytes))
			break
		}
		output = []byte(strings.TrimSpace(responsePayload.Choices[0].Text))

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
}
