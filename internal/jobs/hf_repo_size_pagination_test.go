package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFetchHFRepoTreeFollowsPagination pins the fix for a #840-review finding:
// an unpaginated fetch would silently UNDERSTATE requiredBytes on a repo with
// more files than one API page -- a fail-open in the wrong direction, since a
// real shortfall could then slip through the preflight. This spins up a fake
// HF tree API across 3 pages (via the RFC 5988 Link header HF actually uses,
// confirmed empirically against the live API) and asserts every entry across
// all pages is returned.
func TestFetchHFRepoTreeFollowsPagination(t *testing.T) {
	pages := [][]hfTreeEntry{
		{{Type: "file", Path: "a.safetensors", Size: 1}, {Type: "file", Path: "b.safetensors", Size: 2}},
		{{Type: "file", Path: "c.safetensors", Size: 3}},
		{{Type: "file", Path: "d.safetensors", Size: 4}},
	}

	var srv *httptest.Server
	requests := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		requests++
		idx := 0
		if page != "" {
			fmt.Sscanf(page, "%d", &idx)
		}
		if idx+1 < len(pages) {
			next := fmt.Sprintf("%s%s?page=%d", srv.URL, r.URL.Path, idx+1)
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(pages[idx])
	}))
	defer srv.Close()

	origBase := hfTreeAPIBase
	hfTreeAPIBase = srv.URL
	t.Cleanup(func() { hfTreeAPIBase = origBase })

	entries, err := fetchHFRepoTree(context.Background(), "some/repo")
	if err != nil {
		t.Fatalf("fetchHFRepoTree: %v", err)
	}
	if requests != 3 {
		t.Errorf("expected 3 page requests, got %d", requests)
	}
	if len(entries) != 4 {
		t.Fatalf("expected all 4 entries across 3 pages, got %d: %v", len(entries), entries)
	}
	var total int64
	for _, e := range entries {
		total += e.fileSize()
	}
	if total != 1+2+3+4 {
		t.Errorf("total size across paginated entries = %d, want %d", total, 1+2+3+4)
	}
}

// TestFetchHFRepoTreeSinglePageNoLinkHeader confirms the common case (no
// pagination needed) still works exactly as before -- most real model repos
// fit in one page (empirically verified against black-forest-labs/FLUX.1-dev,
// which returns no Link header at all at the default page size).
func TestFetchHFRepoTreeSinglePageNoLinkHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]hfTreeEntry{
			{Type: "file", Path: "model.safetensors", Size: 100},
		})
	}))
	defer srv.Close()

	origBase := hfTreeAPIBase
	hfTreeAPIBase = srv.URL
	t.Cleanup(func() { hfTreeAPIBase = origBase })

	entries, err := fetchHFRepoTree(context.Background(), "some/repo")
	if err != nil {
		t.Fatalf("fetchHFRepoTree: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "model.safetensors" {
		t.Errorf("entries = %v, want a single model.safetensors entry", entries)
	}
}

// TestFetchHFRepoTreeCapsRunawayPagination guards against a Link chain that
// never terminates (malformed or hostile server): fetchHFRepoTree must error
// rather than loop forever OR return a truncated/understated tree.
func TestFetchHFRepoTreeCapsRunawayPagination(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always point to "itself" -- an infinite chain.
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, srv.URL+r.URL.Path+"?x=1"))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]hfTreeEntry{{Type: "file", Path: "x", Size: 1}})
	}))
	defer srv.Close()

	origBase := hfTreeAPIBase
	hfTreeAPIBase = srv.URL
	t.Cleanup(func() { hfTreeAPIBase = origBase })

	_, err := fetchHFRepoTree(context.Background(), "some/repo")
	if err == nil {
		t.Fatal("expected an error for a Link chain that never terminates, got nil")
	}
}

// TestNextTreePageURL pins the Link-header parsing against the exact format
// HF's live API returns (verified via curl against huggingface.co).
func TestNextTreePageURL(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"empty header", "", ""},
		{
			"HF's actual format",
			`<https://huggingface.co/api/models/black-forest-labs/FLUX.1-dev/tree/main?expand=false&recursive=true&limit=5&cursor=abc123>; rel="next"`,
			"https://huggingface.co/api/models/black-forest-labs/FLUX.1-dev/tree/main?expand=false&recursive=true&limit=5&cursor=abc123",
		},
		{"no next rel present", `<https://example.com/prev>; rel="prev"`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.header != "" {
				h.Set("Link", tt.header)
			}
			if got := nextTreePageURL(h); got != tt.want {
				t.Errorf("nextTreePageURL(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}
