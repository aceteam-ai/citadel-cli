package jobs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestChunkText(t *testing.T) {
	if got := chunkText(""); got != nil {
		t.Fatalf("empty input should yield nil, got %v", got)
	}
	if got := chunkText("   \n\n  "); got != nil {
		t.Fatalf("whitespace-only should yield nil, got %v", got)
	}

	// Small text: single chunk.
	got := chunkText("hello world")
	if len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("small text: got %v", got)
	}

	// Multiple paragraphs under budget coalesce; over budget split.
	para := strings.Repeat("a", 600)
	text := para + "\n\n" + para + "\n\n" + para
	got = chunkText(text)
	if len(got) < 2 {
		t.Fatalf("expected multiple chunks for %d bytes, got %d", len(text), len(got))
	}
	for i, c := range got {
		if strings.TrimSpace(c) == "" {
			t.Fatalf("chunk %d is empty", i)
		}
	}
}

func TestChunkTextOversizedParagraphSplitsOnRuneBoundary(t *testing.T) {
	// A single multibyte paragraph larger than the target must hard-split
	// without corrupting a rune.
	big := strings.Repeat("é", 2000) // 2 bytes/rune → 4000 bytes, one paragraph
	got := chunkText(big)
	if len(got) < 2 {
		t.Fatalf("oversized paragraph should split, got %d chunks", len(got))
	}
	for i, c := range got {
		if !isValidUTF8(c) {
			t.Fatalf("chunk %d has invalid UTF-8 (rune split): %q", i, c[:min(20, len(c))])
		}
	}
}

func TestChunkTextRespectsMaxChunks(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxChunksPerFile+50; i++ {
		b.WriteString(strings.Repeat("z", chunkTargetBytes+10))
		b.WriteString("\n\n")
	}
	got := chunkText(b.String())
	if len(got) > maxChunksPerFile {
		t.Fatalf("chunk count %d exceeds cap %d", len(got), maxChunksPerFile)
	}
}

func TestResolveIndexDBPath(t *testing.T) {
	t.Setenv("CITADEL_INDEX_DB", "")
	if got := resolveIndexDBPath("/explicit/x.db", "/ws"); got != "/explicit/x.db" {
		t.Fatalf("explicit path should win, got %q", got)
	}
	t.Setenv("CITADEL_INDEX_DB", "/env/y.db")
	if got := resolveIndexDBPath("", "/ws"); got != "/env/y.db" {
		t.Fatalf("env path should win over default, got %q", got)
	}
	t.Setenv("CITADEL_INDEX_DB", "")
	if got := resolveIndexDBPath("", "/home/u/citadel-node/workspace"); got != "/home/u/citadel-node/index.db" {
		t.Fatalf("default should sit beside workspace parent, got %q", got)
	}
}

func TestFileIndexMissingPath(t *testing.T) {
	h := NewFileIndexHandler(t.TempDir(), filepath.Join(t.TempDir(), "i.db"))
	_, err := h.Execute(JobContext{}, &nexus.Job{ID: "j", Type: "FILE_INDEX", Payload: map[string]string{}})
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("expected missing-path error, got %v", err)
	}
}

func TestFileSemanticSearchMissingQuery(t *testing.T) {
	h := NewFileSemanticSearchHandler(t.TempDir(), filepath.Join(t.TempDir(), "i.db"))
	_, err := h.Execute(JobContext{}, &nexus.Job{ID: "j", Type: "FILE_SEMANTIC_SEARCH", Payload: map[string]string{}})
	if err == nil || !strings.Contains(err.Error(), "query") {
		t.Fatalf("expected missing-query error, got %v", err)
	}
}

// fakeTEI stands in for the node's TEI embedding service. It returns a
// deterministic 3-dim "embedding" keyed off whether the text mentions "cat" or
// "database", so the round-trip test can assert semantic ranking.
func fakeTEI(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/health") {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type dataItem struct {
			Object    string    `json:"object"`
			Embedding []float64 `json:"embedding"`
			Index     int       `json:"index"`
		}
		resp := map[string]any{"object": "list", "model": req.Model}
		var data []dataItem
		for i, in := range req.Input {
			lo := strings.ToLower(in)
			vec := []float64{0.05, 0.05, 0.05}
			switch {
			case strings.Contains(lo, "cat"), strings.Contains(lo, "kitten"), strings.Contains(lo, "feline"):
				vec = []float64{1, 0, 0}
			case strings.Contains(lo, "database"), strings.Contains(lo, "sql"), strings.Contains(lo, "query"):
				vec = []float64{0, 1, 0}
			}
			data = append(data, dataItem{Object: "embedding", Embedding: vec, Index: i})
		}
		resp["data"] = data
		resp["usage"] = map[string]int{"prompt_tokens": 1, "total_tokens": 1}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFileIndexAndSemanticSearchRoundTrip(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_DB", "")

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "cats.md"), []byte("The cat sat on the mat. A kitten is a small feline."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "db.md"), []byte("A database stores rows. SQL is a query language."), 0o644); err != nil {
		t.Fatal(err)
	}
	// A binary file must be skipped, not indexed.
	if err := os.WriteFile(filepath.Join(ws, "blob.bin"), []byte{0, 1, 2, 0, 3}, 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "index.db")
	idx := NewFileIndexHandler(ws, dbPath)
	out, err := idx.Execute(JobContext{}, &nexus.Job{ID: "j1", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
	if err != nil {
		t.Fatalf("FILE_INDEX: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal index result: %v", err)
	}
	if res["files_indexed"].(float64) != 2 {
		t.Fatalf("expected 2 files indexed, got %v (result=%s)", res["files_indexed"], out)
	}

	// Re-index unchanged: everything skipped, nothing re-embedded.
	out2, err := idx.Execute(JobContext{}, &nexus.Job{ID: "j2", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
	if err != nil {
		t.Fatalf("FILE_INDEX rerun: %v", err)
	}
	_ = json.Unmarshal(out2, &res)
	if res["files_indexed"].(float64) != 0 || res["files_skipped"].(float64) < 2 {
		t.Fatalf("incremental re-index should skip unchanged files, got %s", out2)
	}

	// Semantic search for a feline query should rank cats.md first.
	search := NewFileSemanticSearchHandler(ws, dbPath)
	sout, err := search.Execute(JobContext{}, &nexus.Job{ID: "j3", Type: "FILE_SEMANTIC_SEARCH", Payload: map[string]string{"query": "tell me about my cat"}})
	if err != nil {
		t.Fatalf("FILE_SEMANTIC_SEARCH: %v", err)
	}
	var sres struct {
		Hits []struct {
			Path  string  `json:"path"`
			Score float64 `json:"score"`
		} `json:"hits"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(sout, &sres); err != nil {
		t.Fatalf("unmarshal search result: %v", err)
	}
	if sres.Count == 0 {
		t.Fatalf("expected hits, got none: %s", sout)
	}
	if !strings.HasSuffix(sres.Hits[0].Path, "cats.md") {
		t.Fatalf("cat query should rank cats.md first, got %s", sres.Hits[0].Path)
	}

	// Prune: delete a file on disk and re-index; its entry must be removed.
	if err := os.Remove(filepath.Join(ws, "db.md")); err != nil {
		t.Fatal(err)
	}
	out3, err := idx.Execute(JobContext{}, &nexus.Job{ID: "j4", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
	if err != nil {
		t.Fatalf("FILE_INDEX prune: %v", err)
	}
	_ = json.Unmarshal(out3, &res)
	if res["files_removed"].(float64) != 1 {
		t.Fatalf("expected 1 file pruned, got %s", out3)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
