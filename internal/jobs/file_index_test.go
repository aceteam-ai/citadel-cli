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
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body io.ReadCloser = io.NopCloser(strings.NewReader(""))
		switch req.URL.Path {
		case "/info":
			body = io.NopCloser(strings.NewReader(`{"max_client_batch_size":32}`))
		case "/v1/embeddings":
			body = &cancelOnEOFBody{
				Reader: strings.NewReader(`{"data":[{"index":0,"embedding":[1]}],"model":"gte"}`),
				cancel: cancel,
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
	_, err = h.Execute(JobContext{Ctx: ctx}, &nexus.Job{ID: "cancel-after-embed", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
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
