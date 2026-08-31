// internal/jobs/model_cache_evict.go
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/cacheindex"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/services"
)

// removeCacheIndexEntry is the shared call-site helper for a whole-entry
// eviction (ollama, HF-hub): best-effort, never fails the eviction that
// already succeeded. Mirrors upsertCacheIndexEntry's logging shape.
func removeCacheIndexEntry(ctx JobContext, jobID, cacheDir, model string) {
	store := cacheIndexFn()
	if store == nil {
		return
	}
	if err := store.Remove(cacheDir, model); err != nil {
		ctx.Log("warn", "     - [Job %s] cache index update failed removing %s/%s: %v", jobID, cacheDir, model, err)
	}
}

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
	// cacheMutationMu (citadel #682 P5, design doc §10.4): see the identical
	// comment on ModelCachePullHandler.Execute. MODEL_CACHE_EVICT is on the
	// same serialized lane as MODEL_CACHE_PULL (#908/deadline.go), so this
	// only ever actually contends with a concurrent GC pass.
	cacheMutationMu.Lock()
	defer cacheMutationMu.Unlock()

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
	case "vllm":
		return h.evictHuggingFace(ctx, job.ID, modelName, engine)
	case "llamacpp":
		// Routed separately from vllm (citadel #906 / #682 P1): llamacpp's
		// cache is a flat directory of raw GGUF files, not the HF hub-cache
		// blob layout evictHuggingFace resolves via hfCacheDir -- routing
		// llamacpp through evictHuggingFace always failed with "not found in
		// HuggingFace cache" (safe, but confusing, and doesn't actually free
		// any space).
		return h.evictLlamaCppGGUF(ctx, job.ID, modelName)
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

	removeCacheIndexEntry(ctx, jobID, services.EngineCacheDirs["ollama"].Dir, modelName)

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

	removeCacheIndexEntry(ctx, jobID, services.HFHubCacheDirName, modelName)

	result := modelCacheEvictResult{
		Status:    "evicted",
		ModelName: modelName,
		Engine:    engine,
	}
	return json.Marshal(result)
}

// evictLlamaCppGGUF removes a raw GGUF file from llamaCppCacheDir() (citadel
// #906 / #682 P1) -- the counterpart to evictHuggingFace for the GGUF-layout
// engine family (llamaCppCacheDir is defined in model_cache_pull.go, same
// package).
//
// The raw, flat directory carries no repo provenance (that's what the
// durable cache index, #682 P2, would add), so this only supports the ONE
// unambiguous case: modelName names an exact, existing file (by basename)
// directly under llamaCppCacheDir() -- exactly what LLAMACPP_MODEL and the
// compose mount expect. Anything else (an HF repo id with no matching bare
// filename present) is a clear, honest error rather than a guess at which
// file(s) to remove.
func (h *ModelCacheEvictHandler) evictLlamaCppGGUF(ctx JobContext, jobID, modelName string) ([]byte, error) {
	dir := llamaCppCacheDir()
	base := filepath.Base(modelName)
	candidate := filepath.Join(dir, base)

	// filepath.Base already collapses any path traversal in modelName down to
	// a bare filename, but guard explicitly anyway: refuse rather than
	// silently resolve outside dir on an unexpected input shape (e.g.
	// modelName == ".." or "").
	if base == "." || base == ".." || base == string(filepath.Separator) || filepath.Dir(candidate) != filepath.Clean(dir) {
		return nil, fmt.Errorf("invalid model name %q for llamacpp eviction", modelName)
	}

	ctx.Log("info", "     - [Job %s] Evicting GGUF file '%s' from llamacpp cache", jobID, base)

	fi, err := os.Stat(candidate)
	if err != nil || fi.IsDir() {
		return nil, fmt.Errorf("model %q not found as a file in the llamacpp GGUF cache (%s); "+
			"llamacpp eviction only supports removing an exact cached filename", modelName, dir)
	}

	if err := os.Remove(candidate); err != nil {
		return nil, fmt.Errorf("failed to remove GGUF file %s: %w", candidate, err)
	}

	ctx.Log("info", "     - [Job %s] Removed GGUF file: %s", jobID, candidate)

	// The cache index's Model key for a gguf-dir entry is the REPO id
	// (pull-created) or the bare filename (backfill-created) -- not
	// necessarily `base`, which is only ever a bare filename here (see this
	// function's own doc comment on why eviction only supports an exact
	// filename). Find whichever entry actually recorded `base` in its Files
	// list and remove just that file from it, dropping the entry entirely
	// if it was the last file recorded for it (Store.RemoveFile's contract).
	if store := cacheIndexFn(); store != nil {
		if model, ok := findLlamaCppIndexEntryForFile(store, base); ok {
			if err := store.RemoveFile(services.LlamaCppCacheDirName, model, base); err != nil {
				ctx.Log("warn", "     - [Job %s] cache index update failed removing %s: %v", jobID, base, err)
			}
		}
		// else: no index entry recorded this file (pre-index cache, or
		// backfill has not run yet) -- nothing to remove, not an error.
	}

	result := modelCacheEvictResult{
		Status:    "evicted",
		ModelName: modelName,
		Engine:    "llamacpp",
	}
	return json.Marshal(result)
}

// findLlamaCppIndexEntryForFile scans every llamacpp cache-index entry for
// one whose Files list contains filename, returning its Model key. Needed
// because evictLlamaCppGGUF's caller-supplied modelName is a bare filename,
// while a pull-created entry is keyed by the REPO id (with filename as one
// of possibly several entries in Files) -- see the call site's comment.
func findLlamaCppIndexEntryForFile(store *cacheindex.Store, filename string) (string, bool) {
	for _, e := range store.Snapshot().EntriesByDir()[services.LlamaCppCacheDirName] {
		for _, f := range e.Files {
			if f == filename {
				return e.Model, true
			}
		}
	}
	return "", false
}

// BuildOllamaRmCommand returns the exec.Cmd for removing a model via ollama.
// Exported for testing command construction.
func BuildOllamaRmCommand(modelName string) *exec.Cmd {
	return exec.Command("ollama", "rm", modelName)
}
