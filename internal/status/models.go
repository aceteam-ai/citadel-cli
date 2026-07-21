package status

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ModelDiscoveryTimeout bounds a single model-discovery probe. Discovery runs
// on the heartbeat's collection cycle (~30s) and inside `citadel status`, so a
// slow/hung engine must never stall the whole collection — callers wrap their
// context with this deadline and treat failure as "no models reported".
const ModelDiscoveryTimeout = 2 * time.Second

// ModelDiscovery provides model discovery for LLM services.
type ModelDiscovery struct {
	httpClient *http.Client
	// host is the hostname used to reach the engine's local API. Defaults to
	// "localhost"; tests override it to pin the httptest listener address.
	host string
}

// NewModelDiscovery creates a new model discovery instance.
func NewModelDiscovery() *ModelDiscovery {
	return &ModelDiscovery{
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		host: "localhost",
	}
}

// EngineTypeFromName maps a service/app name to a model-discovery engine type
// ("vllm", "ollama", "llamacpp", "bonsai"), or "" when the name is not a known
// serving engine. Order matters: "ollama" contains "llama", so it must be
// checked before the llama.cpp patterns. "bonsai" is kept as its own type (it
// serves the llama.cpp /v1/models API but the heartbeat reports it under its own
// engine name so the gateway can route with backend=bonsai).
func EngineTypeFromName(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "vllm"):
		return "vllm"
	case strings.Contains(n, "ollama"):
		return "ollama"
	case strings.Contains(n, "bonsai"):
		return "bonsai"
	case strings.Contains(n, "llamacpp"), strings.Contains(n, "llama.cpp"), strings.Contains(n, "llama-cpp"):
		return "llamacpp"
	}
	return ""
}

// DiscoverModels queries an LLM service for LOADED models (the model(s)
// currently being served, not merely downloaded). It automatically detects the
// service type and uses the appropriate API. A running engine with no model
// loaded returns an empty slice and nil error.
func (m *ModelDiscovery) DiscoverModels(ctx context.Context, serviceType string, port int) ([]string, error) {
	switch serviceType {
	case "vllm":
		return m.discoverOpenAIModels(ctx, "vLLM", port)
	case "llamacpp":
		// llama.cpp's server exposes the same OpenAI-compatible /v1/models list.
		// It can be up with NO model loaded (router mode / deferred load): that is
		// an empty list, not an error.
		return m.discoverOpenAIModels(ctx, "llama.cpp", port)
	case "bonsai":
		// bonsai (PrismML Bonsai-27B) is served by the llama.cpp fork, so it
		// exposes the identical OpenAI-compatible /v1/models endpoint.
		return m.discoverOpenAIModels(ctx, "bonsai", port)
	case "ollama":
		return m.discoverOllamaModels(ctx, port)
	default:
		return nil, fmt.Errorf("unsupported service type: %s", serviceType)
	}
}

// discoverOpenAIModels queries an OpenAI-compatible API for loaded models.
// Used for vLLM and llama.cpp, both of which expose: GET /v1/models
func (m *ModelDiscovery) discoverOpenAIModels(ctx context.Context, engineLabel string, port int) ([]string, error) {
	url := fmt.Sprintf("http://%s:%d/v1/models", m.host, port)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to query %s models: %w", engineLabel, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned status %d", engineLabel, resp.StatusCode)
	}

	// OpenAI-compatible format:
	// { "data": [{ "id": "model-name", "object": "model" }] }
	var listResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("failed to parse %s response: %w", engineLabel, err)
	}

	models := make([]string, 0, len(listResp.Data))
	for _, model := range listResp.Data {
		models = append(models, model.ID)
	}

	return models, nil
}

// discoverOllamaModels queries Ollama's API for LOADED (running) models.
// Ollama exposes: GET /api/ps — the models currently loaded into memory.
// (/api/tags lists DOWNLOADED models, which is not what the heartbeat needs.)
func (m *ModelDiscovery) discoverOllamaModels(ctx context.Context, port int) ([]string, error) {
	url := fmt.Sprintf("http://%s:%d/api/ps", m.host, port)

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
	// { "models": [{ "name": "llama2:latest", "model": "llama2:latest", ... }] }
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
	case "vllm", "llamacpp", "bonsai":
		// vLLM, llama.cpp, and the bonsai llama.cpp fork all expose GET /health.
		return m.checkHTTPHealth(ctx, port)
	case "ollama":
		return m.checkOllamaHealth(ctx, port)
	default:
		return HealthStatusUnknown, nil
	}
}

// checkHTTPHealth checks engine health via the /health endpoint (vLLM,
// llama.cpp).
func (m *ModelDiscovery) checkHTTPHealth(ctx context.Context, port int) (string, error) {
	url := fmt.Sprintf("http://%s:%d/health", m.host, port)

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
	url := fmt.Sprintf("http://%s:%d/", m.host, port)

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
