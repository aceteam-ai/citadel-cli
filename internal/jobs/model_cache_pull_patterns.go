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

// diffusersLayoutDirs are the subfolders a standard diffusers pipeline needs.
// This is exactly the LTX-Video shape that caused #828: a repo with these
// subfolders ALSO carries several sibling top-level single-file checkpoints
// (alternate quantizations/precisions), and an unfiltered snapshot pull grabs
// all of them.
var diffusersLayoutDirs = []string{"transformer", "vae", "text_encoder", "tokenizer", "scheduler"}

// minDiffusersDirsMatched is how many of diffusersLayoutDirs must be present
// before deriveDiffusersAllowPatterns treats the repo as diffusers-shaped,
// avoiding a false-positive default-filter on a repo that merely happens to
// have one similarly-named folder alongside a single checkpoint.
const minDiffusersDirsMatched = 3

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
// Returns nil when the shape doesn't match (not enough diffusers subfolders,
// or no sibling checkpoint problem to solve), so callers fall back to an
// unfiltered pull unchanged -- this is a narrow, evidence-gated default, not a
// blanket "diffusers models get filtered" rule.
func deriveDiffusersAllowPatterns(entries []hfTreeEntry) []string {
	dirSeen := make(map[string]bool)
	rootCheckpoints := 0
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		if !strings.Contains(e.Path, "/") {
			if strings.HasSuffix(strings.ToLower(e.Path), ".safetensors") {
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
	matched := 0
	patterns := make([]string, 0, len(diffusersLayoutDirs)+3)
	for _, d := range diffusersLayoutDirs {
		if dirSeen[d] {
			matched++
			patterns = append(patterns, d+"/*")
		}
	}
	if matched < minDiffusersDirsMatched {
		return nil
	}
	// Root-level config/index files are tiny and needed to load the pipeline.
	patterns = append(patterns, "*.json", "*.txt")
	return patterns
}
