package status

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ModelDiscovery provides model discovery for LLM services.
type ModelDiscovery struct {
	httpClient *http.Client
}

// NewModelDiscovery creates a new model discovery instance.
func NewModelDiscovery() *ModelDiscovery {
	return &ModelDiscovery{
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// DiscoverModels queries an LLM service for loaded models.
// It automatically detects the service type and uses the appropriate API.
func (m *ModelDiscovery) DiscoverModels(ctx context.Context, serviceType string, port int) ([]string, error) {
	switch serviceType {
	case "vllm":
		return m.discoverVLLMModels(ctx, port)
	case "ollama":
		return m.discoverOllamaModels(ctx, port)
	default:
		return nil, fmt.Errorf("unsupported service type: %s", serviceType)
	}
}

// discoverVLLMModels queries vLLM's OpenAI-compatible API for loaded models.
// vLLM exposes: GET /v1/models
func (m *ModelDiscovery) discoverVLLMModels(ctx context.Context, port int) ([]string, error) {
	url := fmt.Sprintf("http://localhost:%d/v1/models", port)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to query vLLM models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vLLM returned status %d", resp.StatusCode)
	}

	// vLLM returns OpenAI-compatible format:
	// { "data": [{ "id": "model-name", "object": "model" }] }
	var vllmResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&vllmResp); err != nil {
		return nil, fmt.Errorf("failed to parse vLLM response: %w", err)
	}

	models := make([]string, 0, len(vllmResp.Data))
	for _, model := range vllmResp.Data {
		models = append(models, model.ID)
	}

	return models, nil
}

// discoverOllamaModels queries Ollama's API for available models.
// Ollama exposes: GET /api/tags
func (m *ModelDiscovery) discoverOllamaModels(ctx context.Context, port int) ([]string, error) {
	url := fmt.Sprintf("http://localhost:%d/api/tags", port)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to query Ollama models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Ollama returned status %d", resp.StatusCode)
	}

	// Ollama returns:
	// { "models": [{ "name": "llama2:latest", "size": 123456 }] }
	var ollamaResp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return nil, fmt.Errorf("failed to parse Ollama response: %w", err)
	}

	models := make([]string, 0, len(ollamaResp.Models))
	for _, model := range ollamaResp.Models {
		models = append(models, model.Name)
	}

	return models, nil
}

// CheckServiceHealth performs a health check on an LLM service.
func (m *ModelDiscovery) CheckServiceHealth(ctx context.Context, serviceType string, port int) (string, error) {
	switch serviceType {
	case "vllm":
		return m.checkVLLMHealth(ctx, port)
	case "ollama":
		return m.checkOllamaHealth(ctx, port)
	default:
		return HealthStatusUnknown, nil
	}
}

// checkVLLMHealth checks vLLM health via the /health endpoint.
func (m *ModelDiscovery) checkVLLMHealth(ctx context.Context, port int) (string, error) {
	url := fmt.Sprintf("http://localhost:%d/health", port)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return HealthStatusUnknown, err
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return HealthStatusUnhealthy, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return HealthStatusOK, nil
	}

	return HealthStatusDegraded, nil
}

// checkOllamaHealth checks Ollama health via the root endpoint.
func (m *ModelDiscovery) checkOllamaHealth(ctx context.Context, port int) (string, error) {
	url := fmt.Sprintf("http://localhost:%d/", port)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return HealthStatusUnknown, err
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return HealthStatusUnhealthy, nil
	}
	defer resp.Body.Close()

	// Ollama returns "Ollama is running" on success
	if resp.StatusCode == http.StatusOK {
		return HealthStatusOK, nil
	}

	return HealthStatusDegraded, nil
}
