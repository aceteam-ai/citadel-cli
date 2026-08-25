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
	case "vllm", "llamacpp":
		return h.pullHuggingFace(ctx, job.ID, modelName, engine, job.Payload)
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
	marginBytes := parseMinFreeBytes(payload, diskSafetyMarginBytes)

	finalAllow, finalIgnore, blockErr := runDiskPreflight(ctx, modelName, allowPatterns, ignorePatterns, marginBytes)
	if blockErr != nil {
		ctx.Log("error", "     - [Job %s] MODEL_CACHE_PULL blocked for '%s': %v", jobID, modelName, blockErr)
		return nil, blockErr
	}

	ctx.Log("info", "     - [Job %s] Pulling model '%s' via %s for %s", jobID, modelName, bin, engine)

	cmd := BuildHuggingFaceDownloadCommandFiltered(bin, modelName, finalAllow, finalIgnore)
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

// parseMinFreeBytes reads the optional `min_free_bytes` payload field
// (citadel #828's configurable safety margin), falling back to def when
// absent, empty, or unparsable.
func parseMinFreeBytes(payload map[string]string, def int64) int64 {
	raw := strings.TrimSpace(payload["min_free_bytes"])
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
	if requiredBytes <= 0 {
		// No sizeable files matched (or the tree response carried no sizes) --
		// nothing to gate on.
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

// hfCacheBaseDir returns the actual hub-cache directory a repo pull writes
// into, used by the disk preflight's free-space check. Mirrors
// huggingface_hub's own precedence (internal/constants.py):
// HF_HUB_CACHE > (legacy) HUGGINGFACE_HUB_CACHE > "$HF_HOME/hub" >
// ~/.cache/huggingface/hub. Getting this wrong is not cosmetic: an operator
// who points HF_HUB_CACHE at a large secondary disk (the common reason to set
// it at all) would otherwise have free space measured on the ROOT volume,
// inverting the check exactly on the nodes most likely to need it. Falls back
// to "." (rather than hfCacheDir's "" on UserHomeDir failure) because
// nearestExistingDir needs a concrete path to walk up from, not a signal to
// skip the check outright.
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
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".cache", "huggingface", "hub")
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
