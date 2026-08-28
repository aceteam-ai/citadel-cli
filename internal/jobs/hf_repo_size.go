// internal/jobs/hf_repo_size.go
//
// Estimates a HuggingFace repo's download size from file metadata (citadel
// #828), so the disk preflight in disk_space.go has a requiredBytes number
// before a single byte is downloaded.
//
// This is the one part of #828 that requires a network call (there is no
// local/offline way to know a repo's file sizes ahead of the pull). Per the
// issue's own guidance it is kept behind an injectable func var
// (hfRepoTreeFn) so the preflight logic is unit-tested against fixed fixtures
// with no live HTTP — TestPlanDiskPreflight and friends never hit the network.
// A real end-to-end "does this correctly size an actual HF repo" check is a
// manual/integration follow-up, not part of this unit-test suite.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// hfAuthToken returns a best-effort HuggingFace auth token from the
// environment, mirroring huggingface_hub's own precedence (HF_TOKEN first,
// HUGGING_FACE_HUB_TOKEN for backward compat). It does NOT read the stored
// token file (~/.cache/huggingface/token) that `hf auth login` writes -- a
// gated repo authorized only that way still 401s the metadata fetch here,
// which is one more reason the preflight fails open rather than closed on a
// fetch error (fetchHFRepoTree's doc comment).
func hfAuthToken() string {
	if v := os.Getenv("HF_TOKEN"); v != "" {
		return v
	}
	return os.Getenv("HUGGING_FACE_HUB_TOKEN")
}

// hfMetadataTimeout bounds the HF tree-API call. MODEL_CACHE_PULL is in the
// worker watchdog's unbounded tier (see CLAUDE.md's Consume-Loop Watchdog
// section), so nothing else would cap a hung metadata request.
const hfMetadataTimeout = 30 * time.Second

// hfHTTPClient is package-level so hfMetadataTimeout is the only timeout
// source; fetchHFRepoTree still layers a context deadline per call so a
// caller-supplied shorter deadline (job cancellation) is also honored.
var hfHTTPClient = &http.Client{Timeout: hfMetadataTimeout}

// hfLFSInfo mirrors the `lfs` sub-object the HF tree API attaches to
// LFS-tracked files. Its `size` is the resolved (decompressed) file size,
// distinct from the LFS pointer file's own tiny on-disk size.
type hfLFSInfo struct {
	Size int64 `json:"size"`
}

// hfTreeEntry is the subset of the HF `tree` API response this package reads.
// `size` is present and correct for both regular and LFS files in the tree
// endpoint (unlike the plain `models/{repo}` endpoint, which needs
// `?blobs=true` and still nests LFS sizes under `lfs`), but hfLFSInfo.Size is
// consulted as a fallback in case an older API shape omits `size` at the top
// level for LFS entries.
type hfTreeEntry struct {
	Type string     `json:"type"`
	Path string     `json:"path"`
	Size int64      `json:"size"`
	LFS  *hfLFSInfo `json:"lfs,omitempty"`
}

// fileSize returns the best available size for the entry (top-level size,
// falling back to the LFS blob size).
func (e hfTreeEntry) fileSize() int64 {
	if e.Size > 0 {
		return e.Size
	}
	if e.LFS != nil {
		return e.LFS.Size
	}
	return 0
}

// hfRepoTreeFn fetches a HuggingFace repo's full file tree (recursive, main
// revision). Overridable for tests; production wiring is fetchHFRepoTree.
var hfRepoTreeFn = fetchHFRepoTree

// hfTreeAPIBase is the HF tree-API base URL, overridable so tests can point
// fetchHFRepoTree at an httptest server instead of the real huggingface.co
// (used to test pagination without live HTTP).
var hfTreeAPIBase = "https://huggingface.co/api/models"

// hfMaxTreePages bounds how many pages fetchHFRepoTree will follow via the
// tree API's Link-header (RFC 5988) pagination (citadel #840 review: an
// unpaginated fetch would silently UNDERSTATE requiredBytes on a repo with
// more files than one page, which is a fail-open in the WRONG direction --
// a real shortfall could slip through the preflight). Confirmed empirically
// against the live API that a normal model repo (black-forest-labs/FLUX.1-dev,
// ~40 files) gets no Link header at all at the default page size; pagination
// only activates on repos actually big enough to need it. 50 pages is
// generous headroom over that, and existing purely to stop a malformed/
// hostile Link chain from looping forever, not because real repos are
// expected to approach it.
const hfMaxTreePages = 50

// fetchHFRepoTree calls the HF `tree` API (not the model-info API) because it
// is the one that resolves LFS pointer files to their real byte size inline,
// with no extra per-file round trip. It follows the API's Link-header
// pagination to completion (see hfMaxTreePages) so a large repo's estimate is
// never silently truncated.
//
// Known limitations (both degrade to the fail-open path in runDiskPreflight,
// never to a blocked download): the request is unauthenticated except for
// hfAuthToken()'s best-effort token, so a gated repo the operator hasn't
// separately authorized for CAN still 401/403 here even though the actual
// `hf download` succeeds via its own cached credentials -- the preflight
// simply skips itself in that case. And the revision is hardcoded to `main`;
// a repo whose default branch is something else 404s the same way.
func fetchHFRepoTree(ctx context.Context, repo string) ([]hfTreeEntry, error) {
	url := fmt.Sprintf("%s/%s/tree/main?recursive=true", hfTreeAPIBase, repo)
	token := hfAuthToken()

	var all []hfTreeEntry
	for page := 0; page < hfMaxTreePages; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("building huggingface tree request for %s: %w", repo, err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := hfHTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetching huggingface tree for %s: %w", repo, err)
		}

		var entries []hfTreeEntry
		decodeErr := json.NewDecoder(resp.Body).Decode(&entries)
		next := nextTreePageURL(resp.Header)
		statusCode, status := resp.StatusCode, resp.Status
		resp.Body.Close()

		if statusCode != http.StatusOK {
			return nil, fmt.Errorf("huggingface tree API returned %s for %s", status, repo)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("decoding huggingface tree response for %s: %w", repo, decodeErr)
		}
		all = append(all, entries...)
		if next == "" {
			return all, nil
		}
		url = next
	}
	// A malformed/hostile Link chain that never terminates must not silently
	// return a truncated (understated) tree -- that would size a real repo
	// wrong in the direction that lets an actual shortfall through.
	return nil, fmt.Errorf("huggingface tree for %s did not terminate within %d pages", repo, hfMaxTreePages)
}

// nextTreePageURL extracts the rel="next" target from an RFC 5988 Link
// header (the pagination mechanism HF's tree API uses, confirmed empirically:
// `Link: <...&cursor=...>; rel="next"`), or "" when there is no next page.
func nextTreePageURL(h http.Header) string {
	link := h.Get("Link")
	if link == "" {
		return ""
	}
	for _, part := range strings.Split(link, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		urlPart := strings.TrimSpace(segs[0])
		if !strings.HasPrefix(urlPart, "<") || !strings.HasSuffix(urlPart, ">") {
			continue
		}
		for _, attr := range segs[1:] {
			if strings.TrimSpace(attr) == `rel="next"` {
				return strings.Trim(urlPart, "<>")
			}
		}
	}
	return ""
}

// sumFilteredSize totals the byte size of every FILE entry that patternsInclude
// selects. Pure (no I/O) so it is unit-tested directly against fixture
// entries.
func sumFilteredSize(entries []hfTreeEntry, allowPatterns, ignorePatterns []string) int64 {
	var total int64
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		if !patternsInclude(e.Path, allowPatterns, ignorePatterns) {
			continue
		}
		total += e.fileSize()
	}
	return total
}
