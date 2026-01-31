// internal/jobs/extraction_handler.go
package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// ExtractionHandler proxies extraction requests to the local GLiNER2 service.
type ExtractionHandler struct{}

func (h *ExtractionHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	text, textOk := job.Payload["text"]
	schema, schemaOk := job.Payload["schema"]
	if !textOk {
		return nil, fmt.Errorf("job payload missing 'text' field")
	}

	ctx.Log("info", "     - [Job %s] Waiting for GLiNER2 service to become ready...", job.ID)
	if err := h.waitForReady(); err != nil {
		return nil, err
	}
	ctx.Log("info", "     - [Job %s] GLiNER2 service is ready. Running extraction.", job.ID)

	// Build request payload
	requestPayload := map[string]any{
		"text": text,
	}
	if schemaOk && schema != "" {
		// Schema is JSON-encoded in the string payload
		var schemaObj any
		if err := json.Unmarshal([]byte(schema), &schemaObj); err != nil {
			return nil, fmt.Errorf("failed to parse 'schema' as JSON: %w", err)
		}
		requestPayload["schema"] = schemaObj
	}

	reqBody, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := http.Post("http://localhost:8100/extract", "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to GLiNER2 service: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return bodyBytes, fmt.Errorf("GLiNER2 API returned non-200 status: %s", resp.Status)
	}

	return bodyBytes, nil
}

func (h *ExtractionHandler) waitForReady() error {
	healthURL := "http://localhost:8100/health"
	maxWait := 60 * time.Second
	pollInterval := 1 * time.Second
	startTime := time.Now()

	for time.Since(startTime) < maxWait {
		resp, err := http.Get(healthURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("GLiNER2 service did not become ready within %v", maxWait)
}
