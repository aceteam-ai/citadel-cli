// internal/jobs/vllm_inference.go
package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aceboss/citadel-cli/internal/nexus"
)

type VLLMInferenceHandler struct{}

func (h *VLLMInferenceHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	model, modelOk := job.Payload["model"]
	prompt, promptOk := job.Payload["prompt"]
	if !modelOk || !promptOk {
		return nil, fmt.Errorf("job payload missing 'model' or 'prompt' field")
	}

	// --- HEALTH CHECK BLOCK (Preserved as requested) ---
	fmt.Printf("     - [Job %s] Waiting for vLLM service to become ready...\n", job.ID)
	if err := h.waitForVLLMReady(); err != nil {
		return nil, err
	}
	fmt.Printf("     - [Job %s] vLLM service is ready. Running inference on model '%s'\n", job.ID, model)

	// --- INFERENCE LOGIC ---
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
		return nil, fmt.Errorf("failed to connect to vllm service: %w", httpErr)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return bodyBytes, fmt.Errorf("vllm API returned non-200 status: %s", resp.Status)
	}

	var responsePayload struct {
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
	}
	if json.Unmarshal(bodyBytes, &responsePayload) != nil || len(responsePayload.Choices) == 0 {
		return bodyBytes, fmt.Errorf("failed to parse vLLM response or no choices returned")
	}
	return []byte(strings.TrimSpace(responsePayload.Choices[0].Text)), nil
}

func (h *VLLMInferenceHandler) waitForVLLMReady() error {
	vllmHealthURL := "http://localhost:8000/health"
	maxWait := 60 * time.Second
	pollInterval := 1 * time.Second
	startTime := time.Now()

	for time.Since(startTime) < maxWait {
		resp, httpErr := http.Get(vllmHealthURL)
		if httpErr == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil // Service is ready
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("vllm service did not become ready within %v", maxWait)
}
