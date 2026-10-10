package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodeindex"
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

func TestFileIndexBatchesTwoHundredChunksAndContinuesAfterEmbeddingFailure(t *testing.T) {
	var infoCalls int
	var batchSizes []int
	const secretResponse = "SECRET-FILE-CONTENT-ECHO"
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		infoCalls++
		_, _ = w.Write([]byte(`{"max_client_batch_size":32}`))
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode embedding request: %v", err)
		}
		batchSizes = append(batchSizes, len(req.Input))
		if len(req.Input) > 32 {
			http.Error(w, "batch too large", http.StatusRequestEntityTooLarge)
			return
		}
		for _, input := range req.Input {
			if strings.Contains(input, "BAD-FILE-CONTENT") {
				http.Error(w, secretResponse, http.StatusInternalServerError)
				return
			}
		}
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float64{1, float64(i), 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": req.Model})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("CITADEL_TEI_URL", srv.URL)
	t.Setenv("CITADEL_INDEX_HNSW", "false")

	ws := t.TempDir()
	var paragraphs []string
	for i := 0; i < maxChunksPerFile; i++ {
		paragraphs = append(paragraphs, fmt.Sprintf("%04d-%s", i, strings.Repeat("x", chunkTargetBytes-5)))
	}
	if err := os.WriteFile(filepath.Join(ws, "00-large.md"), []byte(strings.Join(paragraphs, "\n\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "01-bad.md"), []byte("BAD-FILE-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "02-good.md"), []byte("good content after the failed file"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs []string
	dbPath := filepath.Join(t.TempDir(), "index.db")
	h := NewFileIndexHandler(ws, dbPath)
	out, err := h.Execute(JobContext{LogFn: func(_ string, msg string) { logs = append(logs, msg) }}, &nexus.Job{
		ID: "index-batches", Type: "FILE_INDEX", Payload: map[string]string{"path": ws},
	})
	if err != nil {
		t.Fatalf("FILE_INDEX: %v", err)
	}
	var result struct {
		FilesIndexed   int `json:"files_indexed"`
		FilesFailed    int `json:"files_failed"`
		ChunksUpserted int `json:"chunks_upserted"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.FilesIndexed != 2 || result.FilesFailed != 1 || result.ChunksUpserted != maxChunksPerFile+1 {
		t.Fatalf("index result = %+v, want indexed=2 failed=1 chunks=%d", result, maxChunksPerFile+1)
	}
	if infoCalls != 1 {
		t.Fatalf("index operation GET /info calls = %d, want 1", infoCalls)
	}
	for _, size := range batchSizes {
		if size > 32 {
			t.Fatalf("observed oversized batch %d (all=%v)", size, batchSizes)
		}
	}
	if strings.Contains(strings.Join(logs, "\n"), secretResponse) || strings.Contains(string(out), secretResponse) {
		t.Fatalf("upstream response body leaked into logs/result: logs=%q result=%s", logs, out)
	}

	store, err := nodeindex.Open(dbPath)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	files, chunks, err := store.Stats()
	_ = store.Close()
	if err != nil {
		t.Fatalf("index stats: %v", err)
	}
	if files != 2 || chunks != maxChunksPerFile+1 {
		t.Fatalf("persisted files/chunks = %d/%d, want 2/%d", files, chunks, maxChunksPerFile+1)
	}

	search := NewFileSemanticSearchHandler(ws, dbPath)
	searchOut, err := search.Execute(JobContext{}, &nexus.Job{ID: "query-operation", Type: "FILE_SEMANTIC_SEARCH", Payload: map[string]string{"query": "good"}})
	if err != nil {
		t.Fatalf("FILE_SEMANTIC_SEARCH: %v", err)
	}
	var searchResult struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(searchOut, &searchResult); err != nil || searchResult.Count == 0 {
		t.Fatalf("semantic query result count/decode = %d/%v, body=%s", searchResult.Count, err, searchOut)
	}
	if infoCalls != 2 {
		t.Fatalf("index + query GET /info calls = %d, want one per operation (2 total)", infoCalls)
	}
}

func TestFileIndexPropagatesOperationCancellation(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.md"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db")).Execute(
		JobContext{Ctx: ctx},
		&nexus.Job{ID: "cancelled", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestFileSemanticSearchDoesNotEchoMalformedTEIResponse(t *testing.T) {
	const secret = "SECRET-QUERY-ECHO"
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"max_client_batch_size":32}`))
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":"` + secret + `"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("CITADEL_TEI_URL", srv.URL)

	h := NewFileSemanticSearchHandler(t.TempDir(), filepath.Join(t.TempDir(), "index.db"))
	_, err := h.Execute(JobContext{}, &nexus.Job{ID: "query-no-echo", Type: "FILE_SEMANTIC_SEARCH", Payload: map[string]string{"query": "secret"}})
	if err == nil {
		t.Fatal("expected malformed TEI response error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("malformed TEI response value leaked through query error: %v", err)
	}
}

func TestFileIndexFailedReplacementPreservesPriorIndexAndContinues(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"max_client_batch_size":32}`))
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, input := range req.Input {
			if strings.Contains(input, "replacement fails") {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		data := make([]map[string]any, len(req.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float64{1, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "gte"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("CITADEL_TEI_URL", srv.URL)
	t.Setenv("CITADEL_INDEX_HNSW", "false")

	ws := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	priorPath := filepath.Join(ws, "00-prior.md")
	oldContent := []byte("old indexed content")
	if err := os.WriteFile(priorPath, oldContent, 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewFileIndexHandler(ws, dbPath)
	job := &nexus.Job{ID: "seed", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}}
	if _, err := h.Execute(JobContext{}, job); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	if err := os.WriteFile(priorPath, []byte("replacement fails"), 0o644); err != nil {
		t.Fatal(err)
	}
	goodPath := filepath.Join(ws, "01-good.md")
	if err := os.WriteFile(goodPath, []byte("later good content"), 0o644); err != nil {
		t.Fatal(err)
	}
	job.ID = "replacement"
	out, err := h.Execute(JobContext{}, job)
	if err != nil {
		t.Fatalf("replacement index: %v", err)
	}
	var result struct {
		FilesIndexed int `json:"files_indexed"`
		FilesFailed  int `json:"files_failed"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.FilesIndexed != 1 || result.FilesFailed != 1 {
		t.Fatalf("result = %+v, want indexed=1 failed=1", result)
	}

	store, err := nodeindex.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gotHash, indexed, err := store.FileHash(priorPath)
	if err != nil || !indexed || gotHash != hashContent(oldContent) {
		t.Fatalf("prior hash/indexed/error = %q/%v/%v, want old hash preserved", gotHash, indexed, err)
	}
	if _, indexed, err := store.FileHash(goodPath); err != nil || !indexed {
		t.Fatalf("following good file indexed/error = %v/%v, want true/nil", indexed, err)
	}
	files, chunks, err := store.Stats()
	if err != nil || files != 2 || chunks != 2 {
		t.Fatalf("persisted files/chunks/error = %d/%d/%v, want 2/2/nil", files, chunks, err)
	}
}

func TestFileIndexLateBatchFailureWritesNoPartialFile(t *testing.T) {
	var embeddingCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"max_client_batch_size":32}`))
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		embeddingCalls++
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if embeddingCalls == 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		data := make([]map[string]any, len(req.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float64{1}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "gte"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("CITADEL_TEI_URL", srv.URL)

	ws := t.TempDir()
	var paragraphs []string
	for i := 0; i < 40; i++ {
		paragraphs = append(paragraphs, fmt.Sprintf("%04d-%s", i, strings.Repeat("x", chunkTargetBytes-5)))
	}
	filePath := filepath.Join(ws, "late-failure.md")
	if err := os.WriteFile(filePath, []byte(strings.Join(paragraphs, "\n\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	out, err := NewFileIndexHandler(ws, dbPath).Execute(JobContext{}, &nexus.Job{ID: "late", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	var result struct {
		FilesIndexed   int `json:"files_indexed"`
		FilesFailed    int `json:"files_failed"`
		ChunksUpserted int `json:"chunks_upserted"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.FilesIndexed != 0 || result.FilesFailed != 1 || result.ChunksUpserted != 0 || embeddingCalls != 2 {
		t.Fatalf("result/calls = %+v/%d, want indexed=0 failed=1 chunks=0 calls=2", result, embeddingCalls)
	}
	store, err := nodeindex.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, indexed, err := store.FileHash(filePath); err != nil || indexed {
		t.Fatalf("failed file indexed/error = %v/%v, want false/nil", indexed, err)
	}
	files, chunks, err := store.Stats()
	if err != nil || files != 0 || chunks != 0 {
		t.Fatalf("persisted files/chunks/error = %d/%d/%v, want 0/0/nil", files, chunks, err)
	}
}

func TestFileIndexLocalEmbeddingTimeoutCountsFailureAndContinues(t *testing.T) {
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"max_client_batch_size":32}`))
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) > 0 && strings.Contains(req.Input[0], "timeout file") {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		data := make([]map[string]any, len(req.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float64{1}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "gte"})
	})
	srv := httptest.NewServer(mux)
	defer func() {
		close(release)
		srv.Close()
	}()
	t.Setenv("CITADEL_TEI_URL", srv.URL)

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "00-timeout.md"), []byte("timeout file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "01-good.md"), []byte("good file"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db"))
	h.embeddingOp = newTEIEmbeddingOperation(srv.URL)
	h.embeddingOp.requestTimeout = 50 * time.Millisecond
	h.embeddingOp.client.Timeout = 50 * time.Millisecond
	out, err := h.Execute(
		JobContext{}, &nexus.Job{ID: "local-timeout", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}},
	)
	if err != nil {
		t.Fatalf("local request timeout must not cancel whole operation: %v", err)
	}
	var result struct {
		FilesIndexed int `json:"files_indexed"`
		FilesFailed  int `json:"files_failed"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.FilesIndexed != 1 || result.FilesFailed != 1 {
		t.Fatalf("result = %+v, want indexed=1 failed=1", result)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type cancelOnEOFBody struct {
	io.Reader
	cancel context.CancelFunc
	once   sync.Once
}

func (b *cancelOnEOFBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		b.once.Do(b.cancel)
	}
	return n, err
}

func (b *cancelOnEOFBody) Close() error { return nil }

func TestFileIndexCancellationAfterFinalEmbedIsFatalBeforePrune(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var soft atomic.Bool
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body io.ReadCloser = io.NopCloser(strings.NewReader(""))
		switch req.URL.Path {
		case "/info":
			body = io.NopCloser(strings.NewReader(`{"max_client_batch_size":32}`))
		case "/v1/embeddings":
			body = &cancelOnEOFBody{
				Reader: strings.NewReader(`{"data":[{"index":0,"embedding":[1]}],"model":"gte"}`),
				cancel: func() {
					soft.Store(true)
					cancel()
				},
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
	})
	op := newTEIEmbeddingOperation("http://tei.test")
	op.client = &http.Client{Transport: transport, Timeout: time.Second}
	op.requestTimeout = time.Second

	ws := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	stalePath := filepath.Join(ws, "stale.md")
	store, err := nodeindex.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFile(stalePath, "old-hash", 0, 1, "gte", 1, []nodeindex.Chunk{{Index: 0, Text: "stale", Embedding: []float32{1}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	currentPath := filepath.Join(ws, "current.md")
	if err := os.WriteFile(currentPath, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := NewFileIndexHandler(ws, dbPath)
	h.embeddingOp = op
	_, err = h.Execute(JobContext{Ctx: ctx, CancelRequested: soft.Load}, &nexus.Job{ID: "cancel-after-embed", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled rather than success", err)
	}
	store, err = nodeindex.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if hash, indexed, err := store.FileHash(stalePath); err != nil || !indexed || hash != "old-hash" {
		t.Fatalf("stale entry hash/indexed/error = %q/%v/%v, want preserved before prune", hash, indexed, err)
	}
	if _, indexed, err := store.FileHash(currentPath); err != nil || indexed {
		t.Fatalf("cancelled current file indexed/error = %v/%v, want false/nil before Upsert", indexed, err)
	}
	files, chunks, err := store.Stats()
	if err != nil || files != 1 || chunks != 1 {
		t.Fatalf("files/chunks/error after cancellation = %d/%d/%v, want prior 1/1 only", files, chunks, err)
	}
}

func TestFileIndexHardCancellationBeatsSoftOnEmbeddingFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var soft atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"max_client_batch_size":32}`)) })
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, _ *http.Request) {
		soft.Store(true)
		cancel()
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("CITADEL_TEI_URL", srv.URL)
	t.Setenv("CITADEL_INDEX_HNSW", "false")
	ws := t.TempDir()
	path := filepath.Join(ws, "a.md")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	_, err := NewFileIndexHandler(ws, dbPath).Execute(
		JobContext{Ctx: ctx, CancelRequested: soft.Load},
		&nexus.Job{ID: "hard-on-embed-failure", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want fatal context.Canceled rather than soft partial", err)
	}
	store, openErr := nodeindex.Open(dbPath)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer store.Close()
	if _, indexed, hashErr := store.FileHash(path); hashErr != nil || indexed {
		t.Fatalf("failed/cancelled row indexed/error=%v/%v, want false/nil", indexed, hashErr)
	}
}

func TestFileIndexHardCancellationBeatsSoftBeforePrune(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var checks atomic.Int64
	soft := func() bool {
		if checks.Add(1) == 3 {
			cancel()
			return true
		}
		return false
	}
	ws := t.TempDir()
	_, err := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db")).Execute(
		JobContext{Ctx: ctx, CancelRequested: soft},
		&nexus.Job{ID: "hard-before-prune", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want fatal context.Canceled rather than soft partial", err)
	}
}

func TestFileIndexRootWalkFailurePreservesPriorRows(t *testing.T) {
	ws := t.TempDir()
	missingRoot := filepath.Join(ws, "missing-root")
	priorPath := filepath.Join(missingRoot, "prior.md")
	dbPath := filepath.Join(t.TempDir(), "index.db")
	store, err := nodeindex.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFile(priorPath, "prior-hash", 0, 1, "gte", 1, []nodeindex.Chunk{{Index: 0, Text: "prior", Embedding: []float32{1}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := NewFileIndexHandler(ws, dbPath).Execute(JobContext{}, &nexus.Job{ID: "missing-root", Type: "FILE_INDEX", Payload: map[string]string{"path": missingRoot}})
	if err == nil || out != nil || !strings.Contains(err.Error(), "index walk failed") {
		t.Fatalf("result/error = %s/%v, want nil/root-walk error", out, err)
	}
	store, err = nodeindex.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if hash, present, err := store.FileHash(priorPath); err != nil || !present || hash != "prior-hash" {
		t.Fatalf("prior hash/present/error = %q/%v/%v, want preserved row", hash, present, err)
	}
}

// TestFileIndexCooperativeCancelCommitsCurrentFileAndSkipsPrune pins the C1
// mid-walk cancel contract (aceteam#10876): a cooperative (graceful) cancel
// observed at a file boundary stops the walk AFTER committing the current file,
// returns status "cancelled" / checkpoint "partial" with partial counts, and
// SKIPS prune (so a not-yet-visited index row is never deleted — the data-loss
// case). CancelRequested flips true once the first file has been embedded, so
// exactly the first file commits and the rest are left for a resume.
func TestFileIndexCooperativeCancelCommitsCurrentFileAndSkipsPrune(t *testing.T) {
	var embedCount atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"max_client_batch_size":32}`))
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		embedCount.Add(1)
		data := make([]map[string]any, len(req.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float64{1, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "gte"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("CITADEL_TEI_URL", srv.URL)
	t.Setenv("CITADEL_INDEX_HNSW", "false")

	ws := t.TempDir()
	// Lexical order a < b < c: a.md embeds+commits, then cancel is observed at b.md.
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte("content of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dbPath := filepath.Join(t.TempDir(), "index.db")

	// Pre-seed an indexed row under the root that is NOT on disk. A COMPLETE walk
	// would prune it; a CANCELLED walk must NOT (prune is skipped), proving no
	// data loss on the partial index.
	preseed := filepath.Join(ws, "zzz-deleted.md")
	store, err := nodeindex.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFile(preseed, "old-hash", 0, 1, "gte", 2, []nodeindex.Chunk{{Index: 0, Text: "old", Embedding: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	h := NewFileIndexHandler(ws, dbPath)
	jc := JobContext{
		// Graceful cancel is requested once the first file has been embedded.
		CancelRequested: func() bool { return embedCount.Load() >= 1 },
	}
	out, err := h.Execute(jc, &nexus.Job{ID: "coop-cancel", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
	if err != nil {
		t.Fatalf("cooperative cancel must return a partial result, not an error: %v", err)
	}
	var res struct {
		FilesIndexed int    `json:"files_indexed"`
		FilesRemoved int    `json:"files_removed"`
		Status       string `json:"status"`
		Checkpoint   string `json:"checkpoint"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.Status != "cancelled" || res.Checkpoint != "partial" {
		t.Fatalf("status/checkpoint = %q/%q, want cancelled/partial (result=%s)", res.Status, res.Checkpoint, out)
	}
	if res.FilesIndexed != 1 {
		t.Fatalf("files_indexed = %d, want 1 (only the current file commits before stopping); result=%s", res.FilesIndexed, out)
	}
	if res.FilesRemoved != 0 {
		t.Fatalf("files_removed = %d, want 0 (prune must be skipped on cancel)", res.FilesRemoved)
	}

	store, err = nodeindex.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// The current file (a.md) committed.
	if _, indexed, err := store.FileHash(filepath.Join(ws, "a.md")); err != nil || !indexed {
		t.Fatalf("a.md indexed/err = %v/%v, want true/nil (current file must commit)", indexed, err)
	}
	// The pre-seeded, not-visited row survived (prune skipped — no data loss).
	if hash, indexed, err := store.FileHash(preseed); err != nil || !indexed || hash != "old-hash" {
		t.Fatalf("pre-seeded row hash/indexed/err = %q/%v/%v, want preserved (prune must be skipped on cancel)", hash, indexed, err)
	}
	// A later file was NOT indexed (the walk stopped).
	if _, indexed, _ := store.FileHash(filepath.Join(ws, "c.md")); indexed {
		t.Fatal("c.md was indexed; the walk should have stopped at the cancel point")
	}
}

// TestFileIndexEmitsProgress pins that FILE_INDEX reports per-file progress with
// a pre-counted total, basenames (never full paths), and a Final terminal
// update.
func TestFileIndexEmitsProgress(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)

	ws := t.TempDir()
	for _, name := range []string{"one.md", "two.md", "three.md"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte("the cat sat in "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var events []ProgressEvent
	jc := JobContext{Progress: func(ev ProgressEvent) { events = append(events, ev) }}
	h := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db"))
	if _, err := h.Execute(jc, &nexus.Job{ID: "progress", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected progress events, got none")
	}
	last := events[len(events)-1]
	if !last.Final {
		t.Errorf("last progress event Final=%v, want true", last.Final)
	}
	if last.Total != 3 {
		t.Errorf("pre-counted total = %d, want 3", last.Total)
	}
	sawCurrent := false
	for _, ev := range events {
		switch ev.Stage {
		case ProgressStagePlan, ProgressStageEmbed, ProgressStageUpsert, ProgressStagePrune:
		default:
			t.Errorf("FILE_INDEX progress stage = %q, want plan/embed/upsert/prune", ev.Stage)
		}
		if ev.Stage == ProgressStage("index") {
			t.Errorf("progress used retired index stage: %+v", ev)
		}
		if ev.Unit != "files" {
			t.Errorf("progress unit = %q, want files", ev.Unit)
		}
		if strings.ContainsRune(ev.Current, os.PathSeparator) {
			t.Errorf("progress current %q leaked a path separator (must be a basename only)", ev.Current)
		}
		if ev.Current != "" {
			sawCurrent = true
		}
	}
	if !sawCurrent {
		t.Error("expected at least one progress event naming the current basename")
	}
}

func TestFileIndexProgressAdvancesOnlyAfterDisposition(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var requests atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"max_client_batch_size":32}`)) })
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		call := requests.Add(1)
		if call == 1 {
			close(started)
			<-release
		}
		for _, input := range req.Input {
			if strings.Contains(input, "embedding fails") {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		data := make([]map[string]any, len(req.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float64{1, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "gte"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer releaseOnce.Do(func() { close(release) })
	t.Setenv("CITADEL_TEI_URL", srv.URL)
	t.Setenv("CITADEL_INDEX_HNSW", "false")

	ws := t.TempDir()
	for name, body := range map[string]string{
		"00-committed.md": "committed content",
		"01-skipped.md":   "",
		"02-failed.md":    "embedding fails",
	} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var progressMu sync.Mutex
	var events []map[string]any
	progress := NewThrottledProgress(func(event map[string]any) {
		progressMu.Lock()
		defer progressMu.Unlock()
		events = append(events, event)
	}, func() time.Time { return time.Unix(0, 0) })
	type executeResult struct {
		out []byte
		err error
	}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	resultCh := make(chan executeResult, 1)
	go func() {
		out, err := NewFileIndexHandler(ws, dbPath).Execute(
			JobContext{Progress: progress},
			&nexus.Job{ID: "disposition-progress", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}},
		)
		resultCh <- executeResult{out: out, err: err}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first embedding request did not start")
	}
	progressMu.Lock()
	activeEvents := append([]map[string]any(nil), events...)
	progressMu.Unlock()
	if len(activeEvents) == 0 {
		t.Fatal("expected active progress before first disposition")
	}
	for _, event := range activeEvents {
		if done, _ := event["done"].(int); done != 0 {
			t.Fatalf("progress advanced before first disposition: %v", activeEvents)
		}
		if percent, ok := event["percent"].(int); ok && percent >= 100 {
			t.Fatalf("active progress reported %d%% before disposition: %v", percent, activeEvents)
		}
	}
	releaseOnce.Do(func() { close(release) })
	var result executeResult
	select {
	case result = <-resultCh:
	case <-time.After(3 * time.Second):
		t.Fatal("index did not finish after release")
	}
	if result.err != nil {
		t.Fatalf("Execute: %v", result.err)
	}
	var payload struct {
		FilesIndexed int `json:"files_indexed"`
		FilesSkipped int `json:"files_skipped"`
		FilesFailed  int `json:"files_failed"`
	}
	if err := json.Unmarshal(result.out, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.FilesIndexed != 1 || payload.FilesSkipped != 1 || payload.FilesFailed != 1 {
		t.Fatalf("dispositions=%+v, want one committed, skipped, and failed", payload)
	}
	progressMu.Lock()
	finalEvents := append([]map[string]any(nil), events...)
	progressMu.Unlock()
	last := finalEvents[len(finalEvents)-1]
	if last["done"] != 3 || last["total"] != 3 || last["percent"] != 100 {
		t.Fatalf("final progress=%v, want done=total=3 percent=100", last)
	}
}

func TestFileIndexHardCancellationBeatsSoftAtInitialBoundary(t *testing.T) {
	t.Setenv("CITADEL_INDEX_HNSW", "false")
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.md"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	hard, cancel := context.WithCancel(context.Background())
	cancel()
	h := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db"))
	_, err := h.Execute(JobContext{Ctx: hard, CancelRequested: func() bool { return true }}, &nexus.Job{
		ID: "hard-before-soft", Type: "FILE_INDEX", Payload: map[string]string{"path": ws},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want hard context.Canceled rather than partial cancellation", err)
	}
}

func TestFileIndexHardCancellationAfterCommittedUpsertIsFatal(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_HNSW", "false")
	ws := t.TempDir()
	path := filepath.Join(ws, "a.md")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	hard, cancel := context.WithCancel(context.Background())
	h := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db"))
	var soft atomic.Bool
	h.afterUpsert = func() { soft.Store(true); cancel() }
	_, err := h.Execute(JobContext{Ctx: hard, CancelRequested: soft.Load}, &nexus.Job{
		ID: "post-upsert-hard", Type: "FILE_INDEX", Payload: map[string]string{"path": ws},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want fatal context.Canceled", err)
	}
	store, openErr := nodeindex.Open(h.DBPath)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer store.Close()
	if _, indexed, hashErr := store.FileHash(path); hashErr != nil || !indexed {
		t.Fatalf("committed row indexed/error=%v/%v, want true/nil", indexed, hashErr)
	}
}

func TestFileIndexSoftCancelAfterFailedCurrentFileCountsFailure(t *testing.T) {
	var requests atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"max_client_batch_size":32}`)) })
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("CITADEL_TEI_URL", srv.URL)
	t.Setenv("CITADEL_INDEX_HNSW", "false")
	ws := t.TempDir()
	for _, name := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte("content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	h := NewFileIndexHandler(ws, dbPath)
	out, err := h.Execute(JobContext{CancelRequested: func() bool { return requests.Load() >= 1 }}, &nexus.Job{
		ID: "failed-soft", Type: "FILE_INDEX", Payload: map[string]string{"path": ws},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var res struct {
		Seen, Failed int
		Status       string
		Checkpoint   string
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	res.Seen = int(raw["files_seen"].(float64))
	res.Failed = int(raw["files_failed"].(float64))
	res.Status, _ = raw["status"].(string)
	res.Checkpoint, _ = raw["checkpoint"].(string)
	if res.Seen != 1 || res.Failed != 1 || res.Status != "cancelled" || res.Checkpoint != "partial" {
		t.Fatalf("result=%s, want seen=1 failed=1 cancelled/partial", out)
	}
	store, err := nodeindex.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, name := range []string{"a.md", "b.md"} {
		if _, indexed, err := store.FileHash(filepath.Join(ws, name)); err != nil || indexed {
			t.Fatalf("%s indexed/error=%v/%v, want false/nil", name, indexed, err)
		}
	}
}

func TestFileIndexDynamicCandidateGrowsProgressDenominator(t *testing.T) {
	ws := t.TempDir()
	laterDir := filepath.Join(ws, "z-later")
	if err := os.Mkdir(laterDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "a.md"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"max_client_batch_size":32}`)) })
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			if err := os.WriteFile(filepath.Join(laterDir, "b.md"), []byte("second"), 0o644); err != nil {
				t.Errorf("write late candidate: %v", err)
			}
		})
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float64{1, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "gte"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("CITADEL_TEI_URL", srv.URL)
	t.Setenv("CITADEL_INDEX_HNSW", "false")
	var projected []map[string]any
	progress := NewThrottledProgress(func(m map[string]any) { projected = append(projected, m) }, func() time.Time { return time.Unix(0, 0) })
	h := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db"))
	out, err := h.Execute(JobContext{Progress: progress}, &nexus.Job{ID: "dynamic", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(string(out), `"files_seen":2`) {
		t.Fatalf("result=%s, want late candidate visited", out)
	}
	sawGrown := false
	for i, ev := range projected {
		if total, _ := ev["total"].(int); i < len(projected)-1 && total >= 2 {
			sawGrown = true
		}
		if i < len(projected)-1 {
			if percent, ok := ev["percent"].(int); ok && percent >= 100 {
				t.Fatalf("non-final event reported %d%%: %v", percent, ev)
			}
		}
	}
	if !sawGrown {
		t.Fatalf("progress denominator never grew for late candidate: %v", projected)
	}
	last := projected[len(projected)-1]
	if last["total"] != 2 || last["percent"] != 100 {
		t.Fatalf("final progress=%v, want total=2 percent=100", last)
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
