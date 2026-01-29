// internal/jobs/llamacpp_inference.go
package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

type LlamaCppInferenceHandler struct{}

// NOTE (from Architect): The logic below still restarts the container for every job.
// This is a direct port of the original logic for the refactoring. The next step
// should be to modify this handler to use the llama.cpp server's API for dynamic
// model loading, eliminating the slow restart process.

func (h *LlamaCppInferenceHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	modelFile, modelOk := job.Payload["model_file"]
	prompt, promptOk := job.Payload["prompt"]
	if !modelOk || !promptOk {
		return nil, fmt.Errorf("job payload missing 'model_file' or 'prompt' field")
	}

	// --- Step 1: Restart the llama.cpp container with the correct model ---
	ctx.Log("info", "     - [Job %s] Configuring llama.cpp to use model '%s'", job.ID, modelFile)
	homeDir := getUserHomeDir()
	composeFile := filepath.Join(homeDir, "citadel-node/services/llamacpp.yml") // Assuming standard path
	newCommand := fmt.Sprintf("--model /models/%s --host 0.0.0.0 --port 8080 --n-gpu-layers -1", modelFile)
	restartCmd := exec.Command("docker", "compose", "-f", composeFile, "up", "-d", "--force-recreate")
	restartCmd.Env = append(os.Environ(), fmt.Sprintf("LLAMACPP_COMMAND=%s", newCommand))

	if output, err := restartCmd.CombinedOutput(); err != nil {
		return output, fmt.Errorf("failed to restart llama.cpp service with new model: %w", err)
	}

	// --- Step 2: Wait for the server to become ready ---
	ctx.Log("info", "     - [Job %s] Waiting for llama.cpp server to initialize...", job.ID)
	if err := h.waitForLlamaCppReady(); err != nil {
		return nil, err
	}

	// --- Step 3: Perform the inference ---
	ctx.Log("info", "     - [Job %s] Running Llama.cpp inference", job.ID)
	return h.performInference(prompt)
}

func (h *LlamaCppInferenceHandler) waitForLlamaCppReady() error {
	llamacppURL := "http://localhost:8080/completion"
	maxWait := 120 * time.Second
	pollInterval := 2 * time.Second
	startTime := time.Now()

	for time.Since(startTime) < maxWait {
		resp, httpErr := http.Post(llamacppURL, "application/json", bytes.NewBuffer([]byte("{}")))
		if httpErr != nil {
			time.Sleep(pollInterval)
			continue
		}
		if resp.StatusCode == http.StatusServiceUnavailable {
			resp.Body.Close()
			time.Sleep(pollInterval)
			continue
		}
		resp.Body.Close()
		return nil // Server is ready
	}
	return fmt.Errorf("llama.cpp server did not become ready within %v", maxWait)
}

func (h *LlamaCppInferenceHandler) performInference(prompt string) ([]byte, error) {
	requestPayload := map[string]interface{}{
		"prompt":      prompt,
		"n_predict":   256,
		"temperature": 0.7,
	}
	reqBody, _ := json.Marshal(requestPayload)
	resp, httpErr := http.Post("http://localhost:8080/completion", "application/json", bytes.NewBuffer(reqBody))
	if httpErr != nil {
		return nil, fmt.Errorf("failed to connect to llama.cpp service: %w", httpErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return bodyBytes, fmt.Errorf("llama.cpp API returned non-200 status: %s", resp.Status)
	}

	var responsePayload map[string]interface{}
	if json.NewDecoder(resp.Body).Decode(&responsePayload) != nil {
		return nil, fmt.Errorf("failed to parse llama.cpp response")
	}
	content, ok := responsePayload["content"].(string)
	if !ok {
		return nil, fmt.Errorf("could not find 'content' in llama.cpp response: %+v", responsePayload)
	}
	return []byte(content), nil
}

func getUserHomeDir() string {
	if sudoUser := platform.GetSudoUser(); sudoUser != "" {
		homeDir, err := platform.HomeDir(sudoUser)
		if err == nil {
			return homeDir
		}
	}
	homeDir, _ := os.UserHomeDir()
	return homeDir
}
