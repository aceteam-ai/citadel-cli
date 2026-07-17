// internal/jobs/model_cache_pull.go
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// ollamaPullTimeout bounds a foreground `ollama pull`. Pulls of large models
// on slow links can legitimately take a long time, so this is generous — the
// bound exists only so a wedged pull cannot pin a job slot forever.
const ollamaPullTimeout = 2 * time.Hour

// runOllamaPull runs `ollama pull <model>` bounded by ollamaPullTimeout.
// Shared by MODEL_CACHE_PULL (pullOllama) and the SERVICE_START native-ollama
// path (ensureOllamaModel, #543) so both pull with the same bounds.
func runOllamaPull(modelName string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(context.Background(), ollamaPullTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ollama", "pull", modelName)
	return cmd.CombinedOutput()
}

// ModelCachePullHandler handles MODEL_CACHE_PULL jobs.
// It pulls model weights into the local cache for the specified engine.
type ModelCachePullHandler struct{}

// modelCachePullResult is the JSON result returned on success.
type modelCachePullResult struct {
	Status    string `json:"status"`
	ModelName string `json:"model_name"`
	SizeBytes int64  `json:"size_bytes"`
	Engine    string `json:"engine"`
}

func (h *ModelCachePullHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
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
		return h.pullOllama(ctx, job.ID, modelName)
	case "vllm", "llamacpp":
		return h.pullHuggingFace(ctx, job.ID, modelName, engine)
	case "bonsai":
		return h.pullBonsai(ctx, job.ID)
	default:
		return nil, fmt.Errorf("unsupported engine %q: must be ollama, vllm, llamacpp, or bonsai", engine)
	}
}

// Bonsai-27B GGUF coordinates. The MODEL_CACHE_PULL for engine "bonsai" pulls
// exactly this one file (NOT the whole repo, which also carries a ~53GB F16 and
// a drafter GGUF) into a fixed local dir the bonsai compose mounts at /models.
const (
	bonsaiRepo     = "prism-ml/Bonsai-27B-gguf"
	bonsaiGGUFFile = "Bonsai-27B-Q1_0.gguf"
)

// bonsaiCacheDir is the fixed local dir the bonsai GGUF is downloaded into. It
// MUST match services/compose/bonsai.yml's `~/citadel-cache/bonsai:/models`
// mount, or the served path (/models/Bonsai-27B-Q1_0.gguf) will not exist.
func bonsaiCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("citadel-cache", "bonsai")
	}
	return filepath.Join(home, "citadel-cache", "bonsai")
}

// pullBonsai downloads the single Bonsai-27B-Q1_0.gguf file via huggingface-cli
// into bonsaiCacheDir(). Deviates from the task's bare command by adding
// --local-dir so the file lands at a predictable path the compose mount can
// serve (the HF hub cache path carries an unpredictable snapshot hash).
func (h *ModelCachePullHandler) pullBonsai(ctx JobContext, jobID string) ([]byte, error) {
	localDir := bonsaiCacheDir()
	ctx.Log("info", "     - [Job %s] Pulling Bonsai GGUF '%s' from %s into %s via huggingface-cli", jobID, bonsaiGGUFFile, bonsaiRepo, localDir)

	cmd := BuildBonsaiDownloadCommand(localDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("huggingface-cli download failed: %w", err)
	}

	var sizeBytes int64
	if fi, statErr := os.Stat(filepath.Join(localDir, bonsaiGGUFFile)); statErr == nil {
		sizeBytes = fi.Size()
	}

	result := modelCachePullResult{
		Status:    "cached",
		ModelName: bonsaiGGUFFile,
		SizeBytes: sizeBytes,
		Engine:    "bonsai",
	}
	return json.Marshal(result)
}

// BuildBonsaiDownloadCommand returns the exec.Cmd that downloads the single
// Bonsai-27B-Q1_0.gguf file into localDir. Exported for testing command
// construction.
func BuildBonsaiDownloadCommand(localDir string) *exec.Cmd {
	return exec.Command("huggingface-cli", "download", bonsaiRepo, bonsaiGGUFFile, "--local-dir", localDir)
}

// pullOllama runs `ollama pull <model>` to cache the model locally.
func (h *ModelCachePullHandler) pullOllama(ctx JobContext, jobID, modelName string) ([]byte, error) {
	ctx.Log("info", "     - [Job %s] Pulling model '%s' via ollama", jobID, modelName)

	output, err := runOllamaPull(modelName)
	if err != nil {
		return output, fmt.Errorf("ollama pull failed: %w", err)
	}

	// Query model size via `ollama list`
	sizeBytes := ollamaModelSize(modelName)

	result := modelCachePullResult{
		Status:    "cached",
		ModelName: modelName,
		SizeBytes: sizeBytes,
		Engine:    "ollama",
	}
	return json.Marshal(result)
}

// ollamaModelSize attempts to get the model size from `ollama list`.
// Returns 0 if the size cannot be determined.
func ollamaModelSize(modelName string) int64 {
	cmd := exec.Command("ollama", "list")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}
	// Parse lines looking for model name. Each line is:
	// NAME  ID  SIZE  MODIFIED
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		// Match model name (first field may include :tag)
		name := fields[0]
		if name == modelName || strings.HasPrefix(name, modelName+":") {
			// SIZE field is at index 2, with unit at index 3
			// e.g. "4.1 GB"
			if len(fields) >= 4 {
				return parseHumanSize(fields[2], fields[3])
			}
		}
	}
	return 0
}

// parseHumanSize converts human-readable size (e.g. "4.1" "GB") to bytes.
func parseHumanSize(numStr, unit string) int64 {
	var num float64
	if _, err := fmt.Sscanf(numStr, "%f", &num); err != nil {
		return 0
	}
	switch strings.ToUpper(unit) {
	case "B":
		return int64(num)
	case "KB":
		return int64(num * 1024)
	case "MB":
		return int64(num * 1024 * 1024)
	case "GB":
		return int64(num * 1024 * 1024 * 1024)
	case "TB":
		return int64(num * 1024 * 1024 * 1024 * 1024)
	default:
		return 0
	}
}

// pullHuggingFace runs `huggingface-cli download <model>` for vllm/llamacpp engines.
func (h *ModelCachePullHandler) pullHuggingFace(ctx JobContext, jobID, modelName, engine string) ([]byte, error) {
	ctx.Log("info", "     - [Job %s] Pulling model '%s' via huggingface-cli for %s", jobID, modelName, engine)

	cmd := exec.Command("huggingface-cli", "download", modelName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("huggingface-cli download failed: %w", err)
	}

	// Attempt to determine cache size from HuggingFace cache directory.
	sizeBytes := hfCacheModelSize(modelName)

	result := modelCachePullResult{
		Status:    "cached",
		ModelName: modelName,
		SizeBytes: sizeBytes,
		Engine:    engine,
	}
	return json.Marshal(result)
}

// hfCacheModelSize walks the HuggingFace cache directory for the model and
// sums file sizes. Returns 0 if the cache directory cannot be found.
func hfCacheModelSize(modelName string) int64 {
	cacheDir := hfCacheDir(modelName)
	if cacheDir == "" {
		return 0
	}

	var total int64
	filepath.Walk(cacheDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// hfCacheDir returns the HuggingFace cache directory for a model, or empty
// string if it cannot be determined.
func hfCacheDir(modelName string) string {
	// HuggingFace cache follows: ~/.cache/huggingface/hub/models--{org}--{model}/
	base := os.Getenv("HF_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".cache", "huggingface")
	}

	// Convert "org/model" to "models--org--model"
	sanitized := "models--" + strings.ReplaceAll(modelName, "/", "--")
	dir := filepath.Join(base, "hub", sanitized)
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
}

// BuildOllamaPullCommand returns the exec.Cmd for pulling a model via ollama.
// Exported for testing command construction.
func BuildOllamaPullCommand(modelName string) *exec.Cmd {
	return exec.Command("ollama", "pull", modelName)
}

// BuildHuggingFaceDownloadCommand returns the exec.Cmd for downloading a model
// via huggingface-cli. Exported for testing command construction.
func BuildHuggingFaceDownloadCommand(modelName string) *exec.Cmd {
	return exec.Command("huggingface-cli", "download", modelName)
}
