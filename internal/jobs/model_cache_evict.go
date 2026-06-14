// internal/jobs/model_cache_evict.go
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// ModelCacheEvictHandler handles MODEL_CACHE_EVICT jobs.
// It removes cached model weights for the specified engine.
type ModelCacheEvictHandler struct{}

// modelCacheEvictResult is the JSON result returned on success.
type modelCacheEvictResult struct {
	Status    string `json:"status"`
	ModelName string `json:"model_name"`
	Engine    string `json:"engine"`
}

func (h *ModelCacheEvictHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	modelName, ok := job.Payload["model_name"]
	if !ok || modelName == "" {
		return nil, fmt.Errorf("job payload missing 'model_name' field")
	}
	engine, ok := job.Payload["engine"]
	if !ok || engine == "" {
		return nil, fmt.Errorf("job payload missing 'engine' field")
	}

	engine = strings.ToLower(engine)

	switch engine {
	case "ollama":
		return h.evictOllama(ctx, job.ID, modelName)
	case "vllm", "llamacpp":
		return h.evictHuggingFace(ctx, job.ID, modelName, engine)
	default:
		return nil, fmt.Errorf("unsupported engine %q: must be ollama, vllm, or llamacpp", engine)
	}
}

// evictOllama runs `ollama rm <model>` to remove the model from the local cache.
func (h *ModelCacheEvictHandler) evictOllama(ctx JobContext, jobID, modelName string) ([]byte, error) {
	ctx.Log("info", "     - [Job %s] Evicting model '%s' from ollama cache", jobID, modelName)

	cmd := exec.Command("ollama", "rm", modelName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("ollama rm failed: %w", err)
	}

	result := modelCacheEvictResult{
		Status:    "evicted",
		ModelName: modelName,
		Engine:    "ollama",
	}
	return json.Marshal(result)
}

// evictHuggingFace removes the model from the HuggingFace cache directory.
func (h *ModelCacheEvictHandler) evictHuggingFace(ctx JobContext, jobID, modelName, engine string) ([]byte, error) {
	ctx.Log("info", "     - [Job %s] Evicting model '%s' from HuggingFace cache for %s", jobID, modelName, engine)

	cacheDir := hfCacheDir(modelName)
	if cacheDir == "" {
		return nil, fmt.Errorf("model %q not found in HuggingFace cache", modelName)
	}

	if err := os.RemoveAll(cacheDir); err != nil {
		return nil, fmt.Errorf("failed to remove cache directory %s: %w", cacheDir, err)
	}

	ctx.Log("info", "     - [Job %s] Removed cache directory: %s", jobID, cacheDir)

	result := modelCacheEvictResult{
		Status:    "evicted",
		ModelName: modelName,
		Engine:    engine,
	}
	return json.Marshal(result)
}

// BuildOllamaRmCommand returns the exec.Cmd for removing a model via ollama.
// Exported for testing command construction.
func BuildOllamaRmCommand(modelName string) *exec.Cmd {
	return exec.Command("ollama", "rm", modelName)
}
