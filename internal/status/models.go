package status

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/engine"
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
// ("vllm", "ollama", "llamacpp", "bonsai", "unlimited-ocr", "sglang"), or ""
// when the name is not a known serving engine. "bonsai" is kept as its own
// type (it serves the llama.cpp /v1/models API but the heartbeat reports it
// under its own engine name so the gateway can route with backend=bonsai).
//
// citadel #685 slice 2: delegates to internal/engine.TypeFromName, the single
// implementation this function and internal/mesh's own former duplicate
// (internal/mesh/discovery.go, deleted in this same slice) both used to
// hand-maintain independently (design doc §1c).
func EngineTypeFromName(name string) string {
	return engine.TypeFromName(name)
}

// DiscoverModels queries an LLM service for the model(s) it can serve right
// now. It automatically detects the service type and uses the appropriate API.
// A running engine with nothing to serve returns an empty slice and nil error.
//
// "Can serve right now" differs by engine. vLLM, llama.cpp, and bonsai each
// serve exactly the one model they were started with, so their loaded model
// (GET /v1/models) IS the servable set. Ollama is different: it auto-loads any
// pulled model on first request, so every pulled/cached model is immediately
// servable. Ollama therefore reports its available models (GET /api/tags, the
// same set `ollama list` shows), not just the models resident in memory
// (GET /api/ps). Reporting only resident models left a node that had pulled a
// model but not yet served a request advertising an empty model set, so the
// platform registry stayed empty and inference could not route to it
// (citadel-cli#606 / aceteam#6634).
// engineDisplayLabel gives DiscoverModels' discoverOpenAIModels call a
// human-readable engine name for its own error messages, preserved verbatim
// from the pre-migration switch's per-case literals (casing included). Not
// part of internal/engine.EngineSpec -- purely cosmetic, so it stays a small
// local table rather than new registry surface.
var engineDisplayLabel = map[string]string{
	"vllm":          "vLLM",
	"llamacpp":      "llama.cpp",
	"bonsai":        "bonsai",
	"unlimited-ocr": "Unlimited-OCR",
	"sglang":        "SGLang",
}

// DiscoverModels dispatches by the engine's request dialect
// (internal/engine.EngineSpec.Dialect, citadel #685 slice 2) instead of a
// hand-maintained per-engine switch: OllamaNative goes to
// discoverOllamaModels, every other registered dialect (OpenAIChat,
// OpenAICompletions, CompletionsOnly -- sglang) goes to
// discoverOpenAIModels, since all of them expose the same GET /v1/models
// listing. An unregistered name or one with no dialect (tei, diffusers,
// lmstudio, ...) reproduces the old switch's default branch exactly.
//
// llama.cpp can be up with NO model loaded (router mode / deferred load):
// that is an empty list, not an error. Without a sglang/unlimited-ocr
// dialect entry, a running instance would hit the "unsupported service
// type" error below and report permanently "starting" instead of resolving
// its served model (citadel-cli#685 §1b) -- unchanged by this migration,
// just now sourced from internal/engine's dialectByEngine table rather than
// this switch's case list.
func (m *ModelDiscovery) DiscoverModels(ctx context.Context, serviceType string, port int) ([]string, error) {
	eng, ok := engine.Default().Lookup(serviceType)
	if !ok || eng.Spec().Dialect == "" {
		return nil, fmt.Errorf("unsupported service type: %s", serviceType)
	}
	if eng.Spec().Dialect == engine.OllamaNative {
		return m.discoverOllamaModels(ctx, port)
	}
	label := engineDisplayLabel[serviceType]
	if label == "" {
		label = serviceType
	}
	return m.discoverOpenAIModels(ctx, label, port)
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

// discoverOllamaModels queries Ollama's API for AVAILABLE (pulled) models.
// Ollama exposes: GET /api/tags, every model pulled to the node, which is the
// same set `ollama list` shows. Because Ollama auto-loads a pulled model on the
// first request, every pulled model is immediately servable, so /api/tags is the
// correct notion of "what this node can serve" for routing. (GET /api/ps lists
// only models currently resident in memory, which is empty right after a pull
// and before the first request: the gap that made a freshly deployed model
// unroutable, citadel-cli#606.)
func (m *ModelDiscovery) discoverOllamaModels(ctx context.Context, port int) ([]string, error) {
	url := fmt.Sprintf("http://%s:%d/api/tags", m.host, port)

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

// DiscoverEmbeddingModel queries an OpenAI-compatible embedding server for the
// model it is actually serving (citadel-cli#690).
//
// Kept off DiscoverModels deliberately: an embedding server is not a chat
// engine, and routing it through the LLM switch is exactly the mis-attribution
// this fixes. Text Embeddings Inference exposes GET /info with a `model_id`
// field naming the loaded model; it is the only model a TEI process serves, so
// a single-element slice is the whole servable set.
//
// A server that answers but names no model returns an empty slice and nil
// error: running with nothing to report is real, reportable state. Transport
// or parse failures return an error so the caller leaves Models empty rather
// than inventing one.
func (m *ModelDiscovery) DiscoverEmbeddingModel(ctx context.Context, port int) ([]string, error) {
	url := fmt.Sprintf("http://%s:%d/info", m.host, port)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to query embedding server info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding server returned status %d", resp.StatusCode)
	}

	var infoResp struct {
		ModelID string `json:"model_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&infoResp); err != nil {
		return nil, fmt.Errorf("failed to parse embedding server info: %w", err)
	}

	id := strings.TrimSpace(infoResp.ModelID)
	if id == "" {
		return []string{}, nil
	}
	return []string{id}, nil
}

// CheckServiceHealth performs a health check on an LLM service. Like
// DiscoverModels above, this dispatches by dialect (citadel #685 slice 2)
// rather than a hand-maintained switch: OllamaNative goes to
// checkOllamaHealth, every other registered dialect -- vLLM, llama.cpp, the
// bonsai fork, the vLLM-served Unlimited-OCR, and sglang (citadel-cli#685
// §1b; confirmed against internal/worker/llm_readiness.go's engineReadyPath,
// which already probes sglang's readiness at this same path) -- all expose
// GET /health, so they share checkHTTPHealth. An unregistered name or one
// with no dialect reproduces the old switch's default (HealthStatusUnknown)
// exactly.
func (m *ModelDiscovery) CheckServiceHealth(ctx context.Context, serviceType string, port int) (string, error) {
	eng, ok := engine.Default().Lookup(serviceType)
	if !ok || eng.Spec().Dialect == "" {
		return HealthStatusUnknown, nil
	}
	if eng.Spec().Dialect == engine.OllamaNative {
		return m.checkOllamaHealth(ctx, port)
	}
	return m.checkHTTPHealth(ctx, port)
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
