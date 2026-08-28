// internal/jobs/model_cache_pull_patterns.go
//
// Per-model allow_patterns/ignore_patterns for MODEL_CACHE_PULL (citadel
// #828). Both fields are OPTIONAL and additive on the job payload: a payload
// without them behaves exactly as before (pullHuggingFace pulls the full
// repo snapshot). The aceteam backend does not send these fields yet — see
// the PR description's cross-repo follow-up note.
package jobs

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// parseModelCachePullPatterns reads the optional allow_patterns/ignore_patterns
// fields from a MODEL_CACHE_PULL payload. Each field accepts either a JSON
// array string (`["transformer/*","vae/*"]`, matching the convention already
// used for structured payload fields elsewhere, e.g. APPLY_DEVICE_CONFIG's
// `config`) or a plain comma-separated list (`transformer/*,vae/*`) as a
// lenient fallback. Absent or empty input returns nil, nil -- the caller's
// signal to fall back to an unfiltered pull.
func parseModelCachePullPatterns(payload map[string]string) (allowPatterns, ignorePatterns []string) {
	return parsePatternField(payload["allow_patterns"]), parsePatternField(payload["ignore_patterns"])
}

func parsePatternField(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err == nil {
		return cleanPatterns(list)
	}
	return cleanPatterns(strings.Split(raw, ","))
}

func cleanPatterns(list []string) []string {
	out := make([]string, 0, len(list))
	for _, p := range list {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// matchGlob reports whether path matches pattern, using the same semantics
// HuggingFace's own allow_patterns/ignore_patterns filtering uses (Python's
// fnmatch: `*` matches any run of characters INCLUDING `/`, `?` matches
// exactly one character). This must match huggingface_hub's own matcher, not
// shell-glob semantics (Go's filepath.Match, where `*` stops at `/`), or a
// pattern that filters correctly for the CLI's own --include/--exclude flags
// would size a different set of files here than it actually downloads.
func matchGlob(path, pattern string) bool {
	var sb strings.Builder
	sb.WriteByte('^')
	for _, r := range pattern {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteByte('.')
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	sb.WriteByte('$')
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return false
	}
	return re.MatchString(path)
}

// patternsInclude decides whether path survives allow/ignore filtering:
// ignore wins outright; an empty allow list means "everything not ignored";
// a non-empty allow list requires a positive match.
func patternsInclude(path string, allowPatterns, ignorePatterns []string) bool {
	for _, p := range ignorePatterns {
		if matchGlob(path, p) {
			return false
		}
	}
	if len(allowPatterns) == 0 {
		return true
	}
	for _, p := range allowPatterns {
		if matchGlob(path, p) {
			return true
		}
	}
	return false
}

// shardedCheckpointPattern matches the standard HF sharded-checkpoint naming
// convention (e.g. "model-00001-of-00003.safetensors"): several root-level
// files that together ARE one model, as opposed to several independent
// sibling checkpoints (LTX-Video's shape, where each file is a complete,
// alternate model).
var shardedCheckpointPattern = regexp.MustCompile(`-\d+-of-\d+\.safetensors$`)

// isShardedCheckpointName reports whether name looks like one shard of a
// sharded checkpoint rather than a standalone sibling checkpoint.
func isShardedCheckpointName(name string) bool {
	return shardedCheckpointPattern.MatchString(strings.ToLower(name))
}

// knownDiffusersComponentDirs is the set of top-level subfolder names this
// package trusts NOT to be silently droppable if omitted from a derived
// allow-list. It is deliberately broad (covers both UNet pipelines --
// Stable Diffusion 1.x/2.x/XL and derivatives -- and the newer DiT
// pipelines -- Flux/SD3/LTX-Video -- plus SDXL's dual text encoder and the
// auxiliary safety/feature dirs), but it is NOT treated as exhaustive. See
// deriveDiffusersAllowPatterns: this list is only ever used to build a
// CONFIDENCE gate (every dir present must be a member), never to select a
// safe subset out of a possibly-larger, partly-unrecognized set. A PR review
// on #840 caught exactly that flaw: an earlier version derived from ONLY the
// dirs it recognized, so a repo with an unrecognized weight-bearing dir (the
// original version didn't even list `unet/`, the actual weights dir for
// every pre-DiT diffusers pipeline) had that dir silently excluded from both
// the size estimate and the download -- a pull that "succeeds" with missing
// weights and no error anywhere. Extend this list generously; the subset gate
// below is what keeps an incomplete list SAFE rather than merely convenient.
var knownDiffusersComponentDirs = map[string]bool{
	// Weight-bearing pipeline components.
	"transformer":    true, // DiT pipelines: Flux, SD3, LTX-Video, ...
	"unet":           true, // UNet pipelines: SD 1.x/2.x/SDXL and derivatives
	"unet_ema":       true,
	"vae":            true,
	"vae_encoder":    true,
	"vae_decoder":    true,
	"text_encoder":   true,
	"text_encoder_2": true, // SDXL dual text encoder
	"text_encoder_3": true, // SD3 triple text encoder
	"image_encoder":  true,
	"controlnet":     true,
	// Small, non-weight (or negligible) auxiliary components.
	"tokenizer":         true,
	"tokenizer_2":       true,
	"tokenizer_3":       true,
	"scheduler":         true,
	"feature_extractor": true,
	"safety_checker":    true,
	"image_processor":   true,
}

// minRootCheckpointSiblings is the number of top-level *.safetensors files
// required before deriveDiffusersAllowPatterns activates. A single top-level
// checkpoint alongside diffusers subfolders is the ordinary (small) diffusers
// repo shape -- nothing to filter. Multiple siblings (LTX-Video shipped ~13)
// is the multi-checkpoint shape that blows up an unfiltered pull.
const minRootCheckpointSiblings = 2

// deriveDiffusersAllowPatterns implements #828's optional third part: when a
// MODEL_CACHE_PULL payload carries NO explicit allow_patterns/ignore_patterns,
// and the repo's own file tree shows the diffusers-subfolder-plus-sibling-
// checkpoints shape, default to pulling only the pipeline subfolders (plus
// small root config files) instead of everything.
//
// Confidence gate (the load-bearing safety property, per #840's review): this
// derives an allow-list ONLY when EVERY top-level directory in the repo is a
// recognized pipeline component (knownDiffusersComponentDirs) -- i.e. the
// repo's directory set is a SUBSET of what we know is safe to enumerate
// explicitly. The moment even one top-level directory is NOT recognized, this
// bails to nil (pull everything, unfiltered) rather than build an allow-list
// that silently omits it. This is deliberately the opposite of "select the
// dirs we recognize and ignore the rest" -- that was the exact shape of the
// bug this fix replaces (a hardcoded 5-dir list that omitted `unet/`, so a
// Stable-Diffusion-1.5-shaped repo had its actual weights excluded from both
// the size estimate and the download, with no error anywhere). An unmapped
// directory must mean "not confident enough to filter", never "safe to
// drop" -- when this returns nil, the ONLY behavior change is that the
// unfiltered pull proceeds exactly as it did before this PR.
//
// Also returns nil when there's no sibling-checkpoint problem to solve
// (fewer than minRootCheckpointSiblings root .safetensors files) or when the
// root .safetensors files are shards of ONE model
// (isShardedCheckpointName) rather than independent alternates -- filtering
// those out would strip the actual weights the same way an unrecognized
// component dir would.
func deriveDiffusersAllowPatterns(entries []hfTreeEntry) []string {
	dirSeen := make(map[string]bool)
	rootCheckpoints := 0
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		if !strings.Contains(e.Path, "/") {
			if strings.HasSuffix(strings.ToLower(e.Path), ".safetensors") {
				if isShardedCheckpointName(e.Path) {
					// A sharded single model (model-00001-of-00003.safetensors)
					// has multiple root .safetensors files that ARE the model, not
					// sibling alternate checkpoints to discard. Filtering them out
					// would strip the actual weights, so bail on the whole repo
					// rather than risk a "pull succeeded but has no weights" job.
					return nil
				}
				rootCheckpoints++
			}
			continue
		}
		top := e.Path[:strings.IndexByte(e.Path, '/')]
		dirSeen[top] = true
	}
	if rootCheckpoints < minRootCheckpointSiblings {
		return nil
	}
	if len(dirSeen) == 0 {
		return nil
	}
	// Confidence gate: EVERY directory present must be recognized, or bail.
	// No count threshold -- the safety property comes from the subset check,
	// not from how many dirs happen to match.
	for dir := range dirSeen {
		if !knownDiffusersComponentDirs[dir] {
			return nil
		}
	}
	dirNames := make([]string, 0, len(dirSeen))
	for dir := range dirSeen {
		dirNames = append(dirNames, dir)
	}
	sort.Strings(dirNames)
	patterns := make([]string, 0, len(dirNames)+2)
	for _, dir := range dirNames {
		patterns = append(patterns, dir+"/*")
	}
	// Root-level config/index files are tiny and needed to load the pipeline.
	patterns = append(patterns, "*.json", "*.txt")
	return patterns
}
