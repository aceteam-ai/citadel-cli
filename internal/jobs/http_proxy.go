// internal/jobs/http_proxy.go
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

type HTTPProxyHandler struct{}

func (h *HTTPProxyHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	url, ok := job.Payload["url"]
	if !ok {
		return nil, fmt.Errorf("job payload missing 'url' field")
	}
	method := job.Payload["method"]
	if method == "" {
		method = "GET"
	}

	ctx.Log("info", "     - [Job %s] HTTP %s %s", job.ID, method, url)

	var bodyReader io.Reader
	if body, ok := job.Payload["body"]; ok && body != "" {
		bodyReader = bytes.NewBufferString(body)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if headersJSON, ok := job.Payload["headers"]; ok && headersJSON != "" {
		var headers map[string]string
		if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
			return nil, fmt.Errorf("failed to parse headers: %w", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}

	if req.Header.Get("Content-Type") == "" && bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	result := map[string]any{
		"status_code": resp.StatusCode,
		"body":        string(respBody),
	}
	return json.Marshal(result)
}
