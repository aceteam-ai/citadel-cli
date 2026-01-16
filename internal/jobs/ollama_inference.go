// internal/jobs/ollama_inference.go
package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

type OllamaInferenceHandler struct{}

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

func (h *OllamaInferenceHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	model, modelOk := job.Payload["model"]
	prompt, promptOk := job.Payload["prompt"]
	if !modelOk || !promptOk {
		return nil, fmt.Errorf("job payload missing 'model' or 'prompt' field")
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
		return nil, fmt.Errorf("failed to connect to ollama service: %w", httpErr)
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("failed to read ollama response body: %w", readErr)
	}

	if resp.StatusCode != http.StatusOK {
		var ollamaErr ollamaErrorResponse
		if json.Unmarshal(respBody, &ollamaErr) == nil {
			return respBody, fmt.Errorf("ollama API error: %s", ollamaErr.Error)
		}
		return respBody, fmt.Errorf("ollama API returned status %d", resp.StatusCode)
	}

	var ollamaResp ollamaSuccessResponse
	if jsonErr := json.Unmarshal(respBody, &ollamaResp); jsonErr != nil {
		return respBody, fmt.Errorf("failed to parse ollama success response: %w", jsonErr)
	}
	return []byte(ollamaResp.Response), nil
}
