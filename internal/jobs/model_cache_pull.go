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

// pullBonsai downloads the single Bonsai-27B-Q1_0.gguf file via the HuggingFace
// CLI into bonsaiCacheDir(). Deviates from a bare download by adding --local-dir
// so the file lands at a predictable path the compose mount can serve (the HF
// hub cache path carries an unpredictable snapshot hash).
func (h *ModelCachePullHandler) pullBonsai(ctx JobContext, jobID string) ([]byte, error) {
	localDir := bonsaiCacheDir()

	bin, err := resolveHFDownloader()
	if err != nil {
		return nil, err
	}
	ctx.Log("info", "     - [Job %s] Pulling Bonsai GGUF '%s' from %s into %s via %s", jobID, bonsaiGGUFFile, bonsaiRepo, localDir, bin)

	cmd := BuildBonsaiDownloadCommand(bin, localDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("hf download failed: %w", err)
	}

	// No-op detection (citadel #566): the deprecated `huggingface-cli` no-ops on
	// huggingface_hub >= 1.x — it prints a warning, creates --local-dir, and exits
	// 0 WITHOUT downloading. A zero-exit is therefore NOT proof of success; the
	// only reliable signal is the file actually existing with non-zero size.
	sizeBytes, err := verifyDownloadedFile(filepath.Join(localDir, bonsaiGGUFFile))
	if err != nil {
		return output, fmt.Errorf("bonsai pull reported success but produced no file (%w); output: %s", err, strings.TrimSpace(string(output)))
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
// Bonsai-27B-Q1_0.gguf file into localDir using the given HuggingFace CLI binary
// (bin, resolved via resolveHFDownloader). Exported for testing command
// construction.
func BuildBonsaiDownloadCommand(bin, localDir string) *exec.Cmd {
	return exec.Command(bin, hfDownloadArgs(bonsaiRepo, bonsaiGGUFFile, localDir)...)
}

// hfDownloadArgs builds the argument list for a HuggingFace CLI download. Both
// the modern `hf` and the deprecated `huggingface-cli` share the identical
// `download <repo> [file] [--local-dir <dir>]` grammar, so only the binary name
// differs. A non-empty file pulls that single file; an empty file pulls the repo.
// A non-empty localDir materializes into a predictable path (vs the hub cache).
func hfDownloadArgs(repo, file, localDir string) []string {
	args := []string{"download", repo}
	if file != "" {
		args = append(args, file)
	}
	if localDir != "" {
		args = append(args, "--local-dir", localDir)
	}
	return args
}

// resolveHFDownloader locates the HuggingFace download CLI, preferring the modern
// `hf` binary and falling back to the deprecated `huggingface-cli` only if `hf`
// is absent (older envs). CRITICAL (citadel #566): `huggingface-cli` is a no-op
// on huggingface_hub >= 1.x, so `hf` must win whenever it exists.
//
// PATH first (exec.LookPath), then common install locations, because the systemd
// worker's PATH often omits the user's pip/uv bin dirs where huggingface_hub
// installs the CLI (on node 1084 it lives under ~/.uv/python/*/bin). Returns a
// clear error if neither binary can be found anywhere.
func resolveHFDownloader() (string, error) {
	for _, name := range []string{"hf", "huggingface-cli"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
		for _, dir := range hfBinDirs() {
			cand := filepath.Join(dir, name)
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
				return cand, nil
			}
		}
	}
	return "", fmt.Errorf("no HuggingFace CLI found: install the `hf` command (pip install -U huggingface_hub) — neither `hf` nor `huggingface-cli` is on PATH or in a known location")
}

// hfBinDirs returns candidate directories to search for the HuggingFace CLI when
// it is not on PATH. Includes a glob for uv's per-interpreter layout
// (~/.uv/python/*/bin) since that dir has no single stable name.
func hfBinDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return []string{"/usr/local/bin"}
	}
	dirs := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".uv", "python", "bin"),
		filepath.Join(home, "bin"),
		"/usr/local/bin",
	}
	if matches, err := filepath.Glob(filepath.Join(home, ".uv", "python", "*", "bin")); err == nil {
		dirs = append(dirs, matches...)
	}
	return dirs
}

// verifyDownloadedFile returns the size of path if it is a regular, non-empty
// file, or an error otherwise. Used to distinguish a real single-file pull from
// the huggingface-cli no-op (which leaves the --local-dir empty).
func verifyDownloadedFile(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("expected file %s not found: %w", path, err)
	}
	if fi.IsDir() {
		return 0, fmt.Errorf("expected file %s is a directory", path)
	}
	if fi.Size() == 0 {
		return 0, fmt.Errorf("expected file %s is empty", path)
	}
	return fi.Size(), nil
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

// pullHuggingFace runs `hf download <model>` (falling back to the deprecated
// `huggingface-cli download <model>`) for vllm/llamacpp engines. The repo lands
// in the HF hub cache (no --local-dir).
func (h *ModelCachePullHandler) pullHuggingFace(ctx JobContext, jobID, modelName, engine string) ([]byte, error) {
	bin, err := resolveHFDownloader()
	if err != nil {
		return nil, err
	}
	ctx.Log("info", "     - [Job %s] Pulling model '%s' via %s for %s", jobID, modelName, bin, engine)

	cmd := BuildHuggingFaceDownloadCommand(bin, modelName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("hf download failed: %w", err)
	}

	// No-op detection (citadel #566): a zero exit does not prove the download
	// happened — the deprecated huggingface-cli exits 0 without pulling anything.
	// A repo snapshot with zero total bytes means nothing landed, so fail the job.
	sizeBytes := hfCacheModelSize(modelName)
	if sizeBytes == 0 {
		return output, fmt.Errorf("hf download reported success but the model cache for %q is empty — the CLI likely no-oped (deprecated huggingface-cli on huggingface_hub >= 1.x); output: %s", modelName, strings.TrimSpace(string(output)))
	}

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
// repo via the given HuggingFace CLI binary (bin, resolved via
// resolveHFDownloader). Exported for testing command construction.
func BuildHuggingFaceDownloadCommand(bin, modelName string) *exec.Cmd {
	return exec.Command(bin, hfDownloadArgs(modelName, "", "")...)
}
