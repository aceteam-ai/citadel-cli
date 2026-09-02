// internal/jobs/model_cache_pull.go
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cacheindex"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/services"
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
	// Message explains a non-"cached" status (currently only "skipped"). Additive
	// and omitempty, so existing consumers of this JSON are unaffected.
	Message string `json:"message,omitempty"`
}

// selfProvisioningEngines are engines whose compose file OWNS its weights: the
// model id is pinned in the compose and the container downloads it into the
// shared HuggingFace cache mount on first start. There is nothing for this
// handler to fetch, so a MODEL_CACHE_PULL for one of them is a no-op success
// rather than an error (#666).
//
// Why this matters: the platform's deploy path dispatches a MODEL_CACHE_PULL for
// whatever engine it resolved, so every deploy of one of these engines used to
// leave `unsupported engine "tei"` in the node log next to a perfectly
// successful SERVICE_START. Nothing broke -- the pull job's id is not one the
// deploy route subscribes to -- but an error that looks like a real failure
// costs time on every triage that reads these logs.
//
// Membership is not a taste call: an engine belongs here iff its compose pins
// the model AND mounts a cache for the container to download into. That is
// asserted against the embedded compose files in
// TestSelfProvisioningEnginesMatchTheirComposeFiles, so this list cannot drift
// away from what the composes actually do.
//
// Unknown engines still error. This is an explicit allowlist, not a blanket
// "anything unrecognised is fine" -- a typo in the engine name must still fail
// loudly rather than silently report success.
var selfProvisioningEngines = map[string]string{
	"tei":           "the TEI compose pins --model-id and downloads into HUGGINGFACE_HUB_CACHE",
	"diffusers":     "the diffusers compose pins DIFFUSERS_MODEL and downloads into the shared HuggingFace cache",
	"kokoro":        "the kokoro image serves one fixed model and fetches its weights + voices into the mounted cache",
	"transcribe":    "the transcribe compose pins WHISPER_MODEL and faster-whisper downloads it into the shared HuggingFace cache",
	"unlimited-ocr": "the unlimited-ocr compose pins --model and vLLM resolves it from the mounted HuggingFace cache on first start",
	"extraction":    "the extraction compose pins MODEL_NAME and downloads into the shared HuggingFace cache",
}

// IsSelfProvisioningEngine reports whether engine is in selfProvisioningEngines
// -- an engine whose compose file owns its weights, so MODEL_CACHE_PULL is a
// no-op for it. Exported so internal/engine's registry equivalence test
// (citadel #685 slice 1) can verify its own literal copy of this table
// against the real one -- internal/engine cannot import internal/jobs in
// production code (it must stay a leaf importable by internal/status without
// a cycle), so its SelfProvisioning field is a literal translation of this
// map, checked here.
func IsSelfProvisioningEngine(engine string) bool {
	_, ok := selfProvisioningEngines[engine]
	return ok
}

// sortedSelfProvisioningEngines returns the no-op engine names in a stable
// order, so the unsupported-engine error reads the same on every node.
func sortedSelfProvisioningEngines() []string {
	names := make([]string, 0, len(selfProvisioningEngines))
	for name := range selfProvisioningEngines {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// skipSelfProvisioned returns the success result for an engine that provisions
// its own weights. It reports Status "skipped" with a Message naming the reason,
// so an operator reading the job result sees "there was nothing to pull, by
// design" instead of either an error or an unexplained success.
func skipSelfProvisioned(ctx JobContext, jobID, engine, modelName string) ([]byte, error) {
	reason := selfProvisioningEngines[engine]
	ctx.Log("info", "     - [Job %s] MODEL_CACHE_PULL skipped for engine %q: %s", jobID, engine, reason)
	return json.Marshal(modelCachePullResult{
		Status:    "skipped",
		ModelName: modelName,
		Engine:    engine,
		Message:   "nothing to pull: " + reason,
	})
}

func (h *ModelCachePullHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	// cacheMutationMu (citadel #682 P5, design doc §10.4): held for the whole
	// pull so a concurrent P5 GC pass (which only TryLocks) can never race an
	// in-flight download into the same cache dir. MODEL_CACHE_PULL is already
	// on the exec-concurrency-1 serialized lane (#908), so this never
	// contends with another pull/evict -- its only real counterparty is GC,
	// which runs OUTSIDE the lane on its own goroutine. See cache_gc.go's
	// package doc for the full reasoning.
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

	engine = normalizeEngineToken(strings.ToLower(engine))

	switch engine {
	case "ollama":
		return h.pullOllama(ctx, job.ID, modelName)
	case "vllm":
		return h.pullHuggingFace(ctx, job.ID, modelName, engine, job.Payload)
	case "llamacpp":
		// Routed separately from vllm (citadel #906 / #682 P1): llamacpp's
		// compose mounts a raw-GGUF directory, not the HuggingFace hub-cache
		// layout pullHuggingFace writes -- see services/caches.go and
		// pullLlamaCppGGUF's doc comment.
		return h.pullLlamaCppGGUF(ctx, job.ID, modelName, job.Payload)
	case "bonsai":
		return h.pullBonsai(ctx, job.ID)
	default:
		if _, selfProvisioned := selfProvisioningEngines[engine]; selfProvisioned {
			return skipSelfProvisioned(ctx, job.ID, engine, modelName)
		}
		return nil, fmt.Errorf("unsupported engine %q: this handler pulls for ollama, vllm, llamacpp and bonsai; "+
			"engines whose compose owns its weights (%s) are a no-op", engine, strings.Join(sortedSelfProvisioningEngines(), ", "))
	}
}

// normalizeEngineToken canonicalizes engine spellings that the backend's
// provisioning templates emit but that differ from the node's internal engine
// names (citadel#545). The backend's `resolve_model_engine` and the node's
// `services.ServiceMap` disagree on llama.cpp's token: the templates use the
// upstream project's own spelling ("llama.cpp", occasionally "llama-cpp" /
// "llama_cpp"), while the node's compose/service name is "llamacpp". Mirrors
// the equivalent normalization in internal/mesh/discovery.go's engineFromName.
// Every other token (including "diffusers", already a selfProvisioningEngines
// no-op) passes through unchanged.
func normalizeEngineToken(engine string) string {
	switch engine {
	case "llama.cpp", "llama-cpp", "llama_cpp":
		return "llamacpp"
	default:
		return engine
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
// Sourced from services.BonsaiCacheDirName (citadel #906 / #682 P1) rather
// than a second hardcoded "bonsai" literal, so this and
// services/caches_test.go's compose-mount check can never disagree.
func bonsaiCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("citadel-cache", services.BonsaiCacheDirName)
	}
	return filepath.Join(home, "citadel-cache", services.BonsaiCacheDirName)
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

	upsertCacheIndexEntry(ctx, jobID, cacheindex.Entry{
		CacheDir:  services.BonsaiCacheDirName,
		Family:    services.CacheFamilyGGUFDir,
		Model:     bonsaiGGUFFile,
		Engine:    "bonsai",
		Files:     []string{bonsaiGGUFFile},
		SizeBytes: sizeBytes,
	})

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

// llamaCppCacheDirFn resolves llamacpp's canonical raw-GGUF cache directory
// (citadel #906 / #682 P1) -- the counterpart to canonicalHFCacheDir() for
// the GGUF-layout engine family. A package var (like hfCacheModelSizeFn,
// availableDiskBytesFn elsewhere in this package) so a test can redirect it
// away from this machine's real home directory without ever letting a real
// `hf download` subprocess create files under the actual
// ~/citadel-cache/llamacpp.
var llamaCppCacheDirFn = defaultLlamaCppCacheDir

// defaultLlamaCppCacheDir is llamaCppCacheDirFn's production wiring. It MUST
// match services/compose/llamacpp.yml's `~/citadel-cache/llamacpp:/models`
// mount, or a pulled GGUF will not be where LLAMACPP_MODEL/the container
// expects it. Sourced from services.LlamaCppCacheDirName rather than a
// hardcoded "llamacpp" literal, so this and services/caches_test.go's
// compose-mount check can never disagree -- the exact divergence #906
// reports (llamacpp routed through pullHuggingFace, writing into
// ~/citadel-cache/huggingface instead).
func defaultLlamaCppCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("citadel-cache", services.LlamaCppCacheDirName)
	}
	return filepath.Join(home, "citadel-cache", services.LlamaCppCacheDirName)
}

// llamaCppCacheDir is the call-site wrapper around llamaCppCacheDirFn, mirroring
// hfCacheBaseDir/bonsaiCacheDir's shape.
func llamaCppCacheDir() string {
	return llamaCppCacheDirFn()
}

// dirTotalSize sums the sizes of every regular file under dir, recursively.
// Returns 0 if dir cannot be walked (in particular, if it does not exist yet
// -- the normal case before a directory's first-ever pull). Generalizes
// hfCacheModelSize's identical walk-and-sum shape to an arbitrary directory,
// used by pullLlamaCppGGUF's before/after no-op detection since a raw-GGUF
// pull (unlike the HF-hub layout) has no per-model subdirectory to inspect in
// isolation.
func dirTotalSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// alreadyCachedGGUFBytes sums the size of every entry in the repo tree that
// already exists, at its repo-relative path, under dir -- i.e. bytes this
// specific pull would NOT need to (re-)download. Unlike the HF-hub layout's
// hfCacheModelSizeFn (which trusts the hub cache's own models--org--repo
// naming to mean "this belongs to this model"), the raw GGUF directory has no
// such convention -- but a repo-relative PATH match is still exact
// provenance for files hf download's own --local-dir mode would have written
// (it preserves the repo's own file layout under --local-dir), so this needs
// no durable cache index (#682 P2) to be correct for THIS repo's own files.
// It intentionally cannot credit a file that happens to be present under a
// DIFFERENT name/path (e.g. a manually renamed GGUF) -- that is a conscious
// under-credit, not a bug: crediting on name/size alone without a real
// provenance record risks the opposite, worse mistake (treating an unrelated
// file as "already downloaded").
func alreadyCachedGGUFBytes(dir string, entries []hfTreeEntry, allowPatterns, ignorePatterns []string) int64 {
	var total int64
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		if !patternsInclude(e.Path, allowPatterns, ignorePatterns) {
			continue
		}
		local := filepath.Join(dir, filepath.FromSlash(e.Path))
		if fi, err := os.Stat(local); err == nil && !fi.IsDir() {
			total += e.fileSize()
		}
	}
	return total
}

// llamaCppPullSucceeded decides whether a completed llamacpp GGUF pull (a
// `hf download ... --local-dir` subprocess that exited 0) should be treated
// as a real success, given llamaCppCacheDir()'s total size before and after
// the download ran (citadel #906 review). ok==false is pullLlamaCppGGUF's
// #566 no-op-detection failure; sizeBytes is the delta, reported only on
// success.
//
// Deliberately gates on the AFTER state (after > 0), NOT on delta > 0: the
// deprecated `huggingface-cli` no-ops on huggingface_hub >= 1.x (creates
// --local-dir and exits 0 WITHOUT downloading), which is the real case this
// must catch -- a first-ever pull into an empty directory that produces
// nothing (after == 0). But MODEL_CACHE_PULL is dispatched on every deploy,
// and `hf download` is idempotent, so a REDEPLOY of an already-cached GGUF
// repo legitimately re-fetches nothing (delta == 0, after > 0) -- a
// delta-based gate would misreport that success as a no-op failure. Gating
// on "is anything there" mirrors hfCacheModelSize(modelName) == 0 on the
// HF-hub path, which has the identical redeploy-is-a-legitimate-no-op shape.
//
// sizeBytes is clamped at 0 (never negative): dirTotalSize walks the WHOLE
// shared llamacpp directory (not just this pull's files), so a concurrent
// eviction mid-pull could in principle make after < before even though this
// pull itself added bytes; reporting the post-pull total in that edge case
// beats reporting a nonsensical negative size.
func llamaCppPullSucceeded(before, after int64) (sizeBytes int64, ok bool) {
	if after <= 0 {
		return 0, false
	}
	sizeBytes = after - before
	if sizeBytes < 0 {
		sizeBytes = after
	}
	return sizeBytes, true
}

// runGGUFDiskPreflight is llamacpp's disk-space preflight (citadel #906 /
// #682 P1): the same HF repo-metadata size estimate and free-space gate
// runDiskPreflight applies for the HF-hub layout, but pointed at
// llamaCppCacheDir() (the raw-GGUF mount, not the HF hub-cache dir), and
// netted against alreadyCachedGGUFBytes rather than hfCacheModelSizeFn --
// see that function's doc comment for why a repo-relative path match is
// exact provenance here without needing the P2 durable index. Without this
// netting, MODEL_CACHE_PULL (dispatched on every deploy, cached or not) would
// fail closed on the FULL repo size for a redeploy of an already-cached
// model on a disk-tight node -- the same regression citadel #840's review
// flagged (and fixed) for the HF-hub path; a GGUF redeploy must get the same
// protection.
//
// Deliberately does NOT apply deriveDiffusersAllowPatterns's auto-filtering
// (runDiskPreflight's #828-part-3 behavior): that heuristic targets
// diffusers' subfolder-plus-sibling-checkpoints shape, which does not
// describe a GGUF repo, so it is left out rather than risk mis-filtering a
// llamacpp pull.
func runGGUFDiskPreflight(ctx JobContext, modelName string, allowPatterns, ignorePatterns []string, marginBytes int64) (finalAllow, finalIgnore []string, err error) {
	reqCtx, cancel := context.WithTimeout(ctx.Context(), hfMetadataTimeout)
	defer cancel()

	entries, treeErr := hfRepoTreeFn(reqCtx, modelName)
	if treeErr != nil {
		ctx.Log("warn", "     - [Job] disk-space preflight skipped for '%s' (could not fetch repo metadata: %v)", modelName, treeErr)
		return allowPatterns, ignorePatterns, nil
	}

	dir := llamaCppCacheDir()
	requiredBytes := sumFilteredSize(entries, allowPatterns, ignorePatterns)
	if cached := alreadyCachedGGUFBytes(dir, entries, allowPatterns, ignorePatterns); cached > 0 {
		requiredBytes -= cached
		if requiredBytes < 0 {
			requiredBytes = 0
		}
	}
	if requiredBytes <= 0 {
		// Nothing sizeable matched, the tree response carried no sizes, or
		// everything filtered-in is already present locally -- either way,
		// nothing left to gate on (mirrors runDiskPreflight's identical
		// early-return).
		return allowPatterns, ignorePatterns, nil
	}

	statDir := nearestExistingDir(dir)
	available, availErr := availableDiskBytesFn(statDir)
	if availErr != nil {
		ctx.Log("warn", "     - [Job] disk-space preflight skipped for '%s' (could not read free space at %s: %v)", modelName, statDir, availErr)
		return allowPatterns, ignorePatterns, nil
	}

	if planErr := planDiskPreflight(statDir, requiredBytes, int64(available), marginBytes); planErr != nil {
		return nil, nil, planErr
	}
	return allowPatterns, ignorePatterns, nil
}

// pullLlamaCppGGUF downloads model_name's raw GGUF file(s) into
// llamaCppCacheDir() via --local-dir (citadel #906 / #682 P1) -- the same
// idiom pullBonsai already uses for its single fixed file, generalized here
// to an arbitrary bring-your-own-GGUF repo.
//
// This is the fix for the bug #906 reports: before this, llamacpp was
// dispatched through pullHuggingFace, which writes the HuggingFace HUB-cache
// blob layout into ~/citadel-cache/huggingface -- a directory
// services/compose/llamacpp.yml never mounts. llamacpp's compose mounts
// ~/citadel-cache/llamacpp:/models expecting flat, raw GGUF files (see
// services/caches.go), so this pulls into THAT directory with --local-dir,
// which forces the raw layout the same way pullBonsai's --local-dir does
// (huggingface_hub materializes real files at --local-dir instead of the hub
// cache's content-addressed blob store).
//
// The payload's optional allow_patterns/ignore_patterns (#828) apply exactly
// as they do for pullHuggingFace, so a caller that knows the desired
// quantization can target it (e.g. allow_patterns: ["*Q4_K_M.gguf"]) instead
// of pulling every sibling quant in the repo. Absent patterns pull everything
// in the repo, same as an unfiltered pullHuggingFace call.
func (h *ModelCachePullHandler) pullLlamaCppGGUF(ctx JobContext, jobID, modelName string, payload map[string]string) ([]byte, error) {
	bin, err := resolveHFDownloader()
	if err != nil {
		return nil, err
	}

	allowPatterns, ignorePatterns := parseModelCachePullPatterns(payload)
	marginBytes := parseMinHeadroomBytes(payload, diskSafetyMarginBytes)

	finalAllow, finalIgnore, blockErr := runGGUFDiskPreflight(ctx, modelName, allowPatterns, ignorePatterns, marginBytes)
	if blockErr != nil {
		ctx.Log("error", "     - [Job %s] MODEL_CACHE_PULL blocked for '%s': %v", jobID, modelName, blockErr)
		return nil, blockErr
	}

	localDir := llamaCppCacheDir()
	ctx.Log("info", "     - [Job %s] Pulling llamacpp GGUF repo '%s' into %s via %s", jobID, modelName, localDir, bin)

	// No-op detection (citadel #566, generalized) -- see llamaCppPullSucceeded's
	// doc comment for why this gates on the directory's AFTER-pull total, not
	// the before/after delta.
	before := dirTotalSize(localDir)
	beforeFiles := listFilesRelative(localDir)

	cmd := BuildLlamaCppGGUFDownloadCommand(bin, modelName, localDir, finalAllow, finalIgnore)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("hf download failed: %w", err)
	}

	after := dirTotalSize(localDir)
	sizeBytes, ok := llamaCppPullSucceeded(before, after)
	if !ok {
		return output, fmt.Errorf("hf download reported success but %s is empty — the CLI likely no-oped (deprecated huggingface-cli on huggingface_hub >= 1.x); output: %s", localDir, strings.TrimSpace(string(output)))
	}

	recordLlamaCppCacheIndexEntry(ctx, jobID, modelName, finalAllow, finalIgnore, localDir, beforeFiles, sizeBytes)

	result := modelCachePullResult{
		Status:    "cached",
		ModelName: modelName,
		SizeBytes: sizeBytes,
		Engine:    "llamacpp",
	}
	return json.Marshal(result)
}

// listFilesRelative walks dir and returns the set of regular files present,
// keyed by their slash-separated path relative to dir. Used by
// recordLlamaCppCacheIndexEntry's before/after-diff fallback when the repo
// tree re-fetch fails (see that function's doc comment).
func listFilesRelative(dir string) map[string]bool {
	out := map[string]bool{}
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = true
		return nil
	})
	return out
}

// recordLlamaCppCacheIndexEntry writes (or refreshes) this pull's cache
// index entry, keyed by the repo id (modelName) with the COMPLETE set of
// repo-relative files this pull's own files landed at -- design doc §8.1's
// "post-pull intersection" rule: re-fetch the repo tree (the same call
// runGGUFDiskPreflight already made minutes earlier) and keep exactly the
// entries that pass the pull's own allow/ignore patterns AND exist on disk
// now. This is deliberately the FULL current file set, not just what THIS
// pull newly downloaded -- so a redeploy of an already-cached repo (a
// legitimate no-op per llamaCppPullSucceeded's doc comment) still records
// accurate, complete provenance rather than an empty Files list that would
// silently erase what a prior pull already recorded.
//
// Falls back to a before/after directory-listing diff (unioned with
// whatever Files the index already has for this model, if any) when the
// tree re-fetch fails -- coarser (a concurrent pull into the SAME shared
// directory could in principle cross-attribute a file), but the exec-1
// serialized lane (design doc §8.2, and this handler's own
// serializedLaneJobTypes membership) makes a concurrent pull into this
// specific repo's own files not something that happens in practice; this is
// an honest degrade for a rare network hiccup, not a correctness gap this
// handler can close for free.
func recordLlamaCppCacheIndexEntry(ctx JobContext, jobID, modelName string, allowPatterns, ignorePatterns []string, dir string, beforeFiles map[string]bool, sizeBytes int64) {
	store := cacheIndexFn()
	if store == nil {
		return
	}

	var files []string
	reqCtx, cancel := context.WithTimeout(ctx.Context(), hfMetadataTimeout)
	entries, treeErr := hfRepoTreeFn(reqCtx, modelName)
	cancel()
	if treeErr == nil {
		for _, e := range entries {
			if e.Type != "file" || !patternsInclude(e.Path, allowPatterns, ignorePatterns) {
				continue
			}
			if _, statErr := os.Stat(filepath.Join(dir, filepath.FromSlash(e.Path))); statErr == nil {
				files = append(files, e.Path)
			}
		}
	} else {
		after := listFilesRelative(dir)
		union := map[string]bool{}
		for f := range after {
			if !beforeFiles[f] {
				union[f] = true
			}
		}
		if existing, ok := store.Snapshot().Lookup(services.LlamaCppCacheDirName, modelName); ok {
			for _, f := range existing.Files {
				union[f] = true
			}
		}
		for f := range union {
			files = append(files, f)
		}
		sort.Strings(files)
	}

	if err := store.Upsert(cacheindex.Entry{
		CacheDir:  services.LlamaCppCacheDirName,
		Family:    services.CacheFamilyGGUFDir,
		Model:     modelName,
		Engine:    "llamacpp",
		Files:     files,
		SizeBytes: sizeBytes,
	}); err != nil {
		ctx.Log("warn", "     - [Job %s] cache index update failed for llamacpp %q (pull still succeeded): %v", jobID, modelName, err)
	}
}

// BuildLlamaCppGGUFDownloadCommand returns the exec.Cmd that downloads
// modelName's GGUF file(s) into localDir using --local-dir (citadel #906 /
// #682 P1) -- llamacpp's counterpart to
// BuildHuggingFaceDownloadCommandFiltered/BuildBonsaiDownloadCommand.
// Exported for testing command construction.
func BuildLlamaCppGGUFDownloadCommand(bin, modelName, localDir string, allowPatterns, ignorePatterns []string) *exec.Cmd {
	return exec.Command(bin, hfDownloadArgsFiltered(modelName, "", localDir, allowPatterns, ignorePatterns)...)
}

// hfDownloadArgs builds the argument list for a HuggingFace CLI download. Both
// the modern `hf` and the deprecated `huggingface-cli` share the identical
// `download <repo> [file] [--local-dir <dir>]` grammar, so only the binary name
// differs. A non-empty file pulls that single file; an empty file pulls the repo.
// A non-empty localDir materializes into a predictable path (vs the hub cache).
func hfDownloadArgs(repo, file, localDir string) []string {
	return hfDownloadArgsFiltered(repo, file, localDir, nil, nil)
}

// hfDownloadArgsFiltered extends hfDownloadArgs with `--include`/`--exclude`
// glob flags for allow_patterns/ignore_patterns (citadel #828). Both the
// modern `hf` and the deprecated `huggingface-cli` accept repeated
// `--include`/`--exclude` flags mapping directly to `snapshot_download`'s
// `allow_patterns`/`ignore_patterns`. Nil/empty slices add no flags, so this
// is a strict superset of hfDownloadArgs's output — existing callers
// (BuildBonsaiDownloadCommand, and BuildHuggingFaceDownloadCommand's
// no-patterns case) are byte-for-byte unchanged.
func hfDownloadArgsFiltered(repo, file, localDir string, allowPatterns, ignorePatterns []string) []string {
	args := []string{"download", repo}
	if file != "" {
		args = append(args, file)
	}
	if localDir != "" {
		args = append(args, "--local-dir", localDir)
	}
	for _, p := range allowPatterns {
		args = append(args, "--include", p)
	}
	for _, p := range ignorePatterns {
		args = append(args, "--exclude", p)
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

	upsertCacheIndexEntry(ctx, jobID, cacheindex.Entry{
		CacheDir:  services.EngineCacheDirs["ollama"].Dir,
		Family:    services.CacheFamilyNative,
		Model:     modelName,
		Engine:    "ollama",
		SizeBytes: sizeBytes,
	})

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
//
// Two additive, backward-compatible protections were added for citadel #828
// (a snapshot pull of Lightricks/LTX-Video grabbed ~161GB and filled a node's
// disk): the payload's optional allow_patterns/ignore_patterns are threaded
// into the download argv (model_cache_pull_patterns.go), and a free-disk
// preflight runs first (disk_space.go) using a size estimate from the repo's
// own file metadata (hf_repo_size.go). Both steps are best-effort with
// respect to metadata/network availability -- see runDiskPreflight's doc
// comment for why a failure to ESTIMATE fails open while a CONFIRMED
// shortfall fails closed.
func (h *ModelCachePullHandler) pullHuggingFace(ctx JobContext, jobID, modelName, engine string, payload map[string]string) ([]byte, error) {
	bin, err := resolveHFDownloader()
	if err != nil {
		return nil, err
	}

	allowPatterns, ignorePatterns := parseModelCachePullPatterns(payload)
	marginBytes := parseMinHeadroomBytes(payload, diskSafetyMarginBytes)

	finalAllow, finalIgnore, blockErr := runDiskPreflight(ctx, modelName, allowPatterns, ignorePatterns, marginBytes)
	if blockErr != nil {
		ctx.Log("error", "     - [Job %s] MODEL_CACHE_PULL blocked for '%s': %v", jobID, modelName, blockErr)
		return nil, blockErr
	}

	ctx.Log("info", "     - [Job %s] Pulling model '%s' via %s for %s", jobID, modelName, bin, engine)
	warnIfLegacyHFCacheExists(ctx, jobID)

	cmd := buildHFPullCommand(bin, modelName, finalAllow, finalIgnore)
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

	upsertCacheIndexEntry(ctx, jobID, cacheindex.Entry{
		CacheDir:  services.HFHubCacheDirName,
		Family:    services.CacheFamilyHFHub,
		Model:     modelName,
		Engine:    engine,
		Files:     []string{hfHubEntryDirName(modelName)},
		SizeBytes: sizeBytes,
	})

	result := modelCachePullResult{
		Status:    "cached",
		ModelName: modelName,
		SizeBytes: sizeBytes,
		Engine:    engine,
	}
	return json.Marshal(result)
}

// hfHubEntryDirName returns the HF hub-cache "models--org--repo" directory
// name for modelName, relative to the resolved hub dir -- the same
// provenance unit evictHuggingFace's hfCacheDir already resolves (and
// os.RemoveAll's), used here to record the ONE-directory hf-hub cache index
// entry (design doc §8.1). Does not check existence -- callers only use this
// after already confirming the pull produced a non-empty cache via
// hfCacheModelSize, so the directory is known to exist at this point.
func hfHubEntryDirName(modelName string) string {
	return "models--" + strings.ReplaceAll(modelName, "/", "--")
}

// parseMinHeadroomBytes reads the optional `min_headroom_bytes` payload field
// (citadel #828's configurable safety margin), falling back to def when
// absent, empty, or unparsable.
//
// Named (and, per a #840 review WANT, RENAMED from the original
// `min_free_bytes`) to match what this number actually is: headroom required
// ABOVE the estimated download size, not a minimum total-free-disk floor —
// `min_free_bytes` read like the latter, which would have been a materially
// different (and wrong) check once anything actually sent it. Inert today
// (the aceteam backend does not send either key yet), so the rename is a
// no-op in practice and free to make before it matters.
func parseMinHeadroomBytes(payload map[string]string, def int64) int64 {
	raw := strings.TrimSpace(payload["min_headroom_bytes"])
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return def
	}
	return v
}

// runDiskPreflight is the glue between the pure decision (planDiskPreflight)
// and this handler's I/O (HF metadata fetch, disk-free read, logging). It
// returns the patterns pullHuggingFace should actually download with:
//   - the caller's own allowPatterns/ignorePatterns, unchanged, in the common
//     case (metadata fetch unavailable, or the pull fits as requested);
//   - deriveDiffusersAllowPatterns's auto-selected subset when the caller
//     supplied NO patterns AND the repo's shape matches (#828 part 3 -- this
//     is what makes the LTX-Video acceptance case pass with no backend
//     change, since the platform does not send allow_patterns yet);
//   - a non-nil err ONLY when the preflight found a CONFIRMED shortfall; the
//     caller must abort the pull entirely on a non-nil err (patterns are
//     meaningless at that point -- nothing should download).
//
// Deliberately fails OPEN (logs a warning, proceeds with the pull unchanged
// from pre-#828 behavior, err==nil) when the HF metadata fetch or the
// disk-free read itself errors -- a transient HF API hiccup or an
// unsupported statfs platform must not turn a previously-working pull into a
// new failure mode. It fails CLOSED (non-nil err, nothing downloaded) only on
// a positive, confirmed required-bytes-exceeds-available-bytes result, which
// is #828's actual ask.
//
// requiredBytes is netted against hfCacheModelSizeFn(modelName) before the
// gate runs (citadel#840 review), so a redeploy of an already-cached model --
// MODEL_CACHE_PULL is dispatched on every deploy, cached or not -- gates on
// the REMAINING download, not the full repo size. Without this, an
// already-fully-cached model on a now-tight-on-disk node (tight BECAUSE it's
// cached) would fail closed on what used to be a free no-op.
func runDiskPreflight(ctx JobContext, modelName string, allowPatterns, ignorePatterns []string, marginBytes int64) (finalAllow, finalIgnore []string, err error) {
	reqCtx, cancel := context.WithTimeout(ctx.Context(), hfMetadataTimeout)
	defer cancel()

	entries, treeErr := hfRepoTreeFn(reqCtx, modelName)
	if treeErr != nil {
		ctx.Log("warn", "     - [Job] disk-space preflight skipped for '%s' (could not fetch repo metadata: %v)", modelName, treeErr)
		return allowPatterns, ignorePatterns, nil
	}

	finalAllow, finalIgnore = allowPatterns, ignorePatterns
	if len(allowPatterns) == 0 && len(ignorePatterns) == 0 {
		if derived := deriveDiffusersAllowPatterns(entries); derived != nil {
			finalAllow = derived
			ctx.Log("info", "     - [Job] auto-selected diffusers subfolders for '%s' (multi-checkpoint repo layout detected): %s", modelName, strings.Join(derived, ", "))
		}
	}

	requiredBytes := sumFilteredSize(entries, finalAllow, finalIgnore)

	// citadel#840 review (BLOCKING): MODEL_CACHE_PULL is dispatched on every
	// deploy, including a redeploy of a model that's already fully (or
	// partially) present in the local HF hub cache -- and `hf download` into
	// that cache (no --local-dir) is resumable/idempotent: already-present
	// blobs are not re-fetched. Without crediting what's already on disk,
	// requiredBytes here is computed as if downloading entirely from
	// scratch, and because "model already cached" strongly correlates with
	// "disk is tight" (the cache is what made it tight), that turns a
	// previously-trivial no-op redeploy into a preflight failure -- a worse
	// regression than the disk-fill this preflight exists to prevent. Net
	// out the already-cached bytes (reusing hfCacheModelSize, the same walk
	// pullHuggingFace's own post-download no-op check uses) so the gate acts
	// on the REMAINING bytes to download, not the full repo size. Clamped at
	// 0, never negative.
	//
	// This is a deliberately coarse credit, not a byte-exact one: it nets the
	// TOTAL cached size for the model against the CURRENT filtered
	// requirement, without correlating specific files/blobs. A model cached
	// under different patterns than the current request would still net
	// (accurately, since HF's blob store is content-addressed and dedupes
	// across requests) if fully re-coverable, but a partial, unrelated cache
	// could theoretically under-credit or over-credit slightly. Netting the
	// coarse total is still strictly more correct than the pre-fix behavior
	// of crediting nothing at all.
	if cached := hfCacheModelSizeFn(modelName); cached > 0 {
		requiredBytes -= cached
		if requiredBytes < 0 {
			requiredBytes = 0
		}
	}

	if requiredBytes <= 0 {
		// Nothing left to download (fully covered by the local cache), or no
		// sizeable files matched, or the tree response carried no sizes --
		// either way, nothing to gate on.
		return finalAllow, finalIgnore, nil
	}

	dir := nearestExistingDir(hfCacheBaseDir())
	available, availErr := availableDiskBytesFn(dir)
	if availErr != nil {
		ctx.Log("warn", "     - [Job] disk-space preflight skipped for '%s' (could not read free space at %s: %v)", modelName, dir, availErr)
		return finalAllow, finalIgnore, nil
	}

	if planErr := planDiskPreflight(dir, requiredBytes, int64(available), marginBytes); planErr != nil {
		return nil, nil, planErr
	}
	return finalAllow, finalIgnore, nil
}

// canonicalHFCacheDir is the ONE place the canonical, container-mounted
// HuggingFace cache directory is computed (citadel #682 P0). It MUST match
// the container mount every self-provisioning/vllm/llamacpp compose file
// uses (`~/citadel-cache/huggingface:/root/.cache/huggingface` --
// services/compose/vllm.yml et al.), or a host-side pull and the container's
// own reads land in two different places again -- the exact bug this fixes.
//
// Both pullHuggingFace's subprocess env (below) and hfCacheBaseDir's default
// resolve through THIS function rather than each hard-coding the path
// separately, per the citadel #840 review coordination note: setting the
// canonical path only on the exec.Cmd's own Env would leave hfCacheBaseDir
// (a parent-process os.Getenv read, never seeing a child's Env) still
// pointing at the pre-fix host default (~/.cache/huggingface), silently
// reopening the divergence for the disk preflight and the already-cached
// netting (#840) even though the download itself moved.
//
// The subdirectory name is sourced from services.HFHubCacheDirName (citadel
// #906 / #682 P1's canonical cache-path table), not a second hardcoded
// "huggingface" literal, so this function and every OTHER HF-hub-layout
// engine's compose mount (asserted by services/caches_test.go) can never
// disagree.
func canonicalHFCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("citadel-cache", services.HFHubCacheDirName)
	}
	return filepath.Join(home, "citadel-cache", services.HFHubCacheDirName)
}

// hfCacheBaseDir returns the actual hub-cache directory a repo pull writes
// into, used by the disk preflight's free-space check and (via hfCacheDir)
// the already-cached-size lookups. Mirrors huggingface_hub's own precedence
// (constants.py): HF_HUB_CACHE > (legacy) HUGGINGFACE_HUB_CACHE >
// "$HF_HOME/hub" > "$XDG_CACHE_HOME/huggingface/hub" > canonicalHFCacheDir()
// + "/hub". Getting the env-var precedence wrong is not cosmetic: an operator
// who points HF_HUB_CACHE at a large secondary disk (the common reason to set
// it at all) would otherwise have free space measured on the ROOT volume,
// inverting the check exactly on the nodes most likely to need it -- the same
// reasoning extends to XDG_CACHE_HOME, which huggingface_hub consults before
// falling back further. The final fallback (no env vars set at all) is
// canonicalHFCacheDir(), NOT the historical ~/.cache/huggingface/hub --
// citadel #682's whole point is that a bare `hf download` with no HF_HOME
// set must not land somewhere the engine containers can't see, and this is
// the function the disk-space/no-op-detection code trusts to know where that
// is. canonicalHFCacheDir() always returns a concrete path (it degrades to a
// relative one on a UserHomeDir failure rather than erroring), so
// nearestExistingDir always has something to walk up from.
func hfCacheBaseDir() string {
	if v := os.Getenv("HF_HUB_CACHE"); v != "" {
		return v
	}
	if v := os.Getenv("HUGGINGFACE_HUB_CACHE"); v != "" {
		return v
	}
	if base := os.Getenv("HF_HOME"); base != "" {
		return filepath.Join(base, "hub")
	}
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "huggingface", "hub")
	}
	return filepath.Join(canonicalHFCacheDir(), "hub")
}

// hfDownloadEnv returns the environment for the `hf`/`huggingface-cli`
// download subprocess (citadel #682 P0): the process's own environment, plus
// HF_HOME pointed at canonicalHFCacheDir() so a pull with no cache location
// configured lands in the SAME directory the engine containers mount,
// instead of the CLI's own default (~/.cache/huggingface, invisible to any
// container). canonicalHFCacheDir() is the exact function hfCacheBaseDir's
// own default resolves through, so this subprocess's actual write location
// and every parent-process read of "where are the weights" (the disk
// preflight, the post-download no-op check, MODEL_CACHE_EVICT) agree.
//
// Checks the same four env vars in the same order hfCacheBaseDir does, and
// injects nothing if any is already set -- an operator's own HF_HUB_CACHE /
// HUGGINGFACE_HUB_CACHE / HF_HOME / XDG_CACHE_HOME is respected, never
// silently overridden. (A duplicate HF_HOME entry appended after os.Environ()
// would usually be shadowed by the earlier one in practice, but that relies
// on unspecified getenv-with-duplicate-keys behavior across libc
// implementations -- checking explicitly is the portable way to guarantee an
// override always wins, not just usually.)
func hfDownloadEnv() []string {
	for _, k := range []string{"HF_HUB_CACHE", "HUGGINGFACE_HUB_CACHE", "HF_HOME", "XDG_CACHE_HOME"} {
		if os.Getenv(k) != "" {
			return os.Environ()
		}
	}
	return append(os.Environ(), "HF_HOME="+canonicalHFCacheDir())
}

// buildHFPullCommand is pullHuggingFace's actual command constructor: argv
// via BuildHuggingFaceDownloadCommandFiltered, env via hfDownloadEnv(). This
// is the ONE call site that applies the citadel #682 fix to a real pull --
// exported test coverage of hfDownloadEnv()/hfCacheBaseDir() agreeing is not
// by itself proof the running handler uses either; assert on THIS function's
// output, not just its ingredients.
func buildHFPullCommand(bin, modelName string, allowPatterns, ignorePatterns []string) *exec.Cmd {
	cmd := BuildHuggingFaceDownloadCommandFiltered(bin, modelName, allowPatterns, ignorePatterns)
	cmd.Env = hfDownloadEnv()
	return cmd
}

// legacyHFCacheDir is the pre-fix host default `hf download` used to write
// into before citadel #682 -- a directory no engine container ever mounts.
func legacyHFCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "huggingface")
}

// warnIfLegacyHFCacheExists reports, once per pull, whether the pre-fix
// duplicate cache is still present. Per docs/design-cache-ownership.md's P0
// scope ("report the pre-existing duplicate cache once, informationally"):
// this is a log line, not a migration and not the durable/aggregated
// duplicate-detection `citadel status`/heartbeat surfacing that's P3's job --
// existing files at the legacy path are left exactly where they are.
//
// Resolution + the "at least one real models--* entry" gate now live in
// LegacyHFHubDirForScan (below), shared with cacheindex.ReconcileScan's own
// durable legacy-duplicate scan-metadata record (design doc §9.3) -- one
// implementation of the gate, not two that could silently drift.
// Best-effort throughout: any stat/read failure is silently treated as
// "nothing to report", never a pull error.
func warnIfLegacyHFCacheExists(ctx JobContext, jobID string) {
	legacyHub := LegacyHFHubDirForScan()
	if legacyHub == "" {
		return
	}
	dir := filepath.Dir(legacyHub)
	ctx.Log("info", "     - [Job %s] found a pre-existing HuggingFace cache at %s (predates citadel #682's fix) — "+
		"this pull writes to %s instead; the old directory is not touched and can be removed manually to reclaim space",
		jobID, dir, hfCacheBaseDir())
}

// LegacyHFHubDirForScan resolves the pre-#682 legacy HuggingFace hub-cache
// directory (~/.cache/huggingface/hub) for cacheindex.ReconcileScan's
// scan-metadata legacy-duplicate probe (design doc §9.3,
// ScanOptions.LegacyHFHubDir) -- exported so cmd/work.go's ReconcileScan call
// site can compute it without internal/cacheindex (a LEAF package) importing
// internal/jobs.
//
// Checks <legacy>/hub for at least one `models--*` entry, NOT just
// os.Stat(legacyHFCacheDir()) existing. The bare ~/.cache/huggingface
// directory exists on essentially any machine anyone ran `hf auth login` on
// -- it may hold nothing but a `token` file. Reporting on that alone would
// point an operator at their HF credentials directory and tell them it "can
// be removed manually to reclaim space", which is actively harmful advice --
// so this only returns non-empty when there is a real, sizeable model cache
// to report.
//
// Returns "" (skip the probe) when the legacy hub dir and the actual
// resolved hub dir (hfCacheBaseDir(), respecting any operator env override)
// are the SAME directory -- an operator override that happens to point back
// at ~/.cache/huggingface, or a test redirect -- there is no second copy to
// report in that case.
func LegacyHFHubDirForScan() string {
	dir := legacyHFCacheDir()
	if dir == "" {
		return ""
	}
	legacyHub := filepath.Join(dir, "hub")
	actualHub := hfCacheBaseDir()
	if legacyHub == actualHub {
		return ""
	}
	entries, err := os.ReadDir(legacyHub)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "models--") {
			return legacyHub
		}
	}
	return ""
}

// hfCacheModelSizeFn resolves how many bytes of a model are already present
// in the local HF cache. Overridable for tests (citadel#840 review); production
// wiring is hfCacheModelSize -- the SAME function pullHuggingFace's own
// post-download no-op check (above) already calls, reused here (not
// reimplemented) so the preflight's "already cached" notion and the
// no-op-detection's "did anything land" notion never disagree about what
// "cached" means.
var hfCacheModelSizeFn = hfCacheModelSize

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
// string if it cannot be determined. Built on hfCacheBaseDir() (the hub-cache
// resolver, citadel #682/#840) rather than its own separate HF_HOME/default
// logic -- the pre-fix version of this function had its own env lookup and
// its own (different) default, which is exactly how the disk preflight's
// "already cached" netting and this function's own callers (the post-download
// no-op check, MODEL_CACHE_EVICT) could disagree with where a pull actually
// wrote. One resolver, used everywhere a model's on-disk cache path matters.
func hfCacheDir(modelName string) string {
	// HuggingFace cache layout: <hfCacheBaseDir()>/models--{org}--{model}/
	sanitized := "models--" + strings.ReplaceAll(modelName, "/", "--")
	dir := filepath.Join(hfCacheBaseDir(), sanitized)
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
	return BuildHuggingFaceDownloadCommandFiltered(bin, modelName, nil, nil)
}

// BuildHuggingFaceDownloadCommandFiltered is BuildHuggingFaceDownloadCommand
// plus allow_patterns/ignore_patterns (citadel #828), so a diffusers-style
// multi-checkpoint repo (e.g. LTX-Video) can pull only the subfolders a
// deploy actually needs instead of every sibling checkpoint. Nil/empty
// patterns produce the identical command BuildHuggingFaceDownloadCommand does.
func BuildHuggingFaceDownloadCommandFiltered(bin, modelName string, allowPatterns, ignorePatterns []string) *exec.Cmd {
	return exec.Command(bin, hfDownloadArgsFiltered(modelName, "", "", allowPatterns, ignorePatterns)...)
}
