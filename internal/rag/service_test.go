package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTEI stands in for the node's local TEI embedding service. It returns
// deterministic 3-dim vectors keyed on content so a query about "cats" scores
// the cat document highest — enough to prove the index+embed+search round trip
// without a real model. Mirrors the mock in internal/jobs/file_index_test.go.
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

// newTestService points a Service at a temp workspace + temp db, with TEI mocked.
func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_DB", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv("CITADEL_EMBEDDING_MODEL", "")

	ws := t.TempDir()
	svc := New(ws, "")
	return svc, ws
}

func TestServiceIndexThenQuery(t *testing.T) {
	svc, ws := newTestService(t)
	writeFile(t, ws, "cats.md", "The cat sat on the mat. A kitten is a small feline.")
	writeFile(t, ws, "db.md", "A database stores rows. SQL is a query language.")

	idx, err := svc.Index(context.Background(), ws, "")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if idx.FilesIndexed != 2 {
		t.Fatalf("expected 2 files indexed, got %d", idx.FilesIndexed)
	}
	if idx.Model != "gte-multilingual-base" {
		t.Fatalf("expected default model in result, got %q", idx.Model)
	}

	res, err := svc.Query(context.Background(), "tell me about a kitten", 5)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	// The cat document must rank first for a cat query.
	if filepath.Base(res.Hits[0].Path) != "cats.md" {
		t.Fatalf("expected cats.md as top hit, got %q (hits=%+v)", res.Hits[0].Path, res.Hits)
	}
	if res.Provenance != "indexed on this node via gte-multilingual-base" {
		t.Fatalf("unexpected provenance: %q", res.Provenance)
	}
}

func TestServiceIncrementalReindexSkipsUnchanged(t *testing.T) {
	svc, ws := newTestService(t)
	writeFile(t, ws, "a.md", "A database stores rows.")

	first, err := svc.Index(context.Background(), ws, "")
	if err != nil {
		t.Fatalf("first Index: %v", err)
	}
	if first.FilesIndexed != 1 {
		t.Fatalf("expected 1 file indexed, got %d", first.FilesIndexed)
	}

	second, err := svc.Index(context.Background(), ws, "")
	if err != nil {
		t.Fatalf("second Index: %v", err)
	}
	if second.FilesIndexed != 0 || second.FilesSkipped != 1 {
		t.Fatalf("expected unchanged re-index to skip (indexed=%d skipped=%d)", second.FilesIndexed, second.FilesSkipped)
	}
}

func TestServiceStatus(t *testing.T) {
	svc, ws := newTestService(t)

	// Fresh index: zero counts, but the effective model is still reported.
	st, err := svc.Status()
	if err != nil {
		t.Fatalf("Status (empty): %v", err)
	}
	if st.Files != 0 || st.Chunks != 0 {
		t.Fatalf("expected empty index, got files=%d chunks=%d", st.Files, st.Chunks)
	}
	if st.Model != "gte-multilingual-base" {
		t.Fatalf("expected effective model on empty index, got %q", st.Model)
	}

	writeFile(t, ws, "a.md", "A database stores rows. SQL is a query language.")
	if _, err := svc.Index(context.Background(), ws, ""); err != nil {
		t.Fatalf("Index: %v", err)
	}
	st, err = svc.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Files != 1 {
		t.Fatalf("expected 1 indexed file, got %d", st.Files)
	}
	if st.Chunks < 1 {
		t.Fatalf("expected >=1 chunk, got %d", st.Chunks)
	}
	if st.LastIndexed == "" {
		t.Fatal("expected a last_indexed timestamp after indexing")
	}
	if st.DBPath == "" {
		t.Fatal("expected a resolved db path in status")
	}
}

func TestServiceModelOverride(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_DB", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv("CITADEL_EMBEDDING_MODEL", "")

	svc := New(t.TempDir(), "custom-embed-v2")
	if svc.Model() != "custom-embed-v2" {
		t.Fatalf("override not applied: %q", svc.Model())
	}
	if svc.Provenance() != "indexed on this node via custom-embed-v2" {
		t.Fatalf("provenance did not reflect override: %q", svc.Provenance())
	}
}

func TestServiceQueryEmptyRejected(t *testing.T) {
	svc, _ := newTestService(t)
	if _, err := svc.Query(context.Background(), "", 5); err == nil {
		t.Fatal("expected an error for empty query")
	}
}

func TestServiceWorkspaceBoundary(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_DB", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv("CITADEL_EMBEDDING_MODEL", "")

	ws := t.TempDir()
	outside := t.TempDir() // a sibling dir outside the workspace
	writeFile(t, outside, "secret.md", "A database of secrets.")

	// Mesh-safe default (New): indexing a path outside the workspace is refused.
	safe := New(ws, "")
	if _, err := safe.Index(context.Background(), outside, ""); err == nil {
		t.Fatal("expected New() to refuse indexing a path outside the workspace")
	}

	// Local operator (NewLocal): the same outside path is permitted.
	local := NewLocal(ws, "")
	res, err := local.Index(context.Background(), outside, "")
	if err != nil {
		t.Fatalf("NewLocal should permit outside path: %v", err)
	}
	if res.FilesIndexed != 1 {
		t.Fatalf("expected 1 file indexed via NewLocal, got %d", res.FilesIndexed)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
