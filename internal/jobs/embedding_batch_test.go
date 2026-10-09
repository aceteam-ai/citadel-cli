package jobs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// resetTEIBatchSizeCache clears the process-wide /info-derived batch-size cache.
// Tests that assert a specific resolved limit must call this first: httptest
// servers can reuse an ephemeral port across tests, so a stale cache entry for a
// reused URL would otherwise leak one test's limit into another.
func resetTEIBatchSizeCache() {
	teiBatchSizeMu.Lock()
	defer teiBatchSizeMu.Unlock()
	teiBatchSizeCache = map[string]int{}
}

// batchTEIStub is a stub TEI service that records every /v1/embeddings batch
// size it sees, rejects any batch larger than maxBatch with HTTP 413 (TEI's
// exact message shape), and returns a 1-dim embedding that encodes the integer
// parsed from each input text so callers can assert end-to-end order.
type batchTEIStub struct {
	srv          *httptest.Server
	maxBatch     int
	mu           sync.Mutex
	requestSizes []int
}

// newBatchTEIStub builds a stub. infoMode selects the GET /info behavior:
//   - "present": serves {"max_client_batch_size": infoLimit}
//   - "zero":    serves {"max_client_batch_size": 0}
//   - "absent":  serves 200 with no max_client_batch_size field
//   - "missing": registers no /info handler (mux 404)
//
// maxBatch (>0) is the size above which /v1/embeddings returns 413.
func newBatchTEIStub(t *testing.T, infoMode string, infoLimit, maxBatch int) *batchTEIStub {
	t.Helper()
	st := &batchTEIStub{maxBatch: maxBatch}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	switch infoMode {
	case "present":
		mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"model_id":"gte","max_client_batch_size":%d}`, infoLimit)
		})
	case "zero":
		mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"model_id":"gte","max_client_batch_size":0}`)
		})
	case "absent":
		mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"model_id":"gte"}`)
		})
	case "missing":
		// no /info route -> mux returns 404
	default:
		t.Fatalf("unknown infoMode %q", infoMode)
	}
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		st.mu.Lock()
		st.requestSizes = append(st.requestSizes, len(req.Input))
		st.mu.Unlock()
		if st.maxBatch > 0 && len(req.Input) > st.maxBatch {
			w.WriteHeader(http.StatusRequestEntityTooLarge) // 413
			fmt.Fprintf(w, `{"message":"batch size %d > maximum allowed batch size %d","code":413,"type":"Validation"}`, len(req.Input), st.maxBatch)
			return
		}
		type dataItem struct {
			Object    string    `json:"object"`
			Embedding []float64 `json:"embedding"`
			Index     int       `json:"index"`
		}
		data := make([]dataItem, len(req.Input))
		for i, in := range req.Input {
			n, _ := strconv.Atoi(in) // non-numeric inputs (chunk text) collapse to 0
			data[i] = dataItem{Object: "embedding", Embedding: []float64{float64(n)}, Index: i}
		}
		// Reverse within the batch so the response is deliberately NOT in index
		// order; a correct client reorders by Index.
		for i, j := 0, len(data)-1; i < j; i, j = i+1, j-1 {
			data[i], data[j] = data[j], data[i]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"model":  req.Model,
			"data":   data,
			"usage":  map[string]int{"prompt_tokens": len(req.Input), "total_tokens": len(req.Input)},
		})
	})
	st.srv = httptest.NewServer(mux)
	t.Cleanup(st.srv.Close)
	return st
}

func (st *batchTEIStub) sizes() []int {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]int, len(st.requestSizes))
	copy(out, st.requestSizes)
	return out
}

// numberedInputs returns ["0","1",...,"n-1"], where each element's integer value
// is both its position and the value the stub embeds, so order can be asserted.
func numberedInputs(n int) []string {
	in := make([]string, n)
	for i := range in {
		in[i] = strconv.Itoa(i)
	}
	return in
}

func TestCallTEIEmbeddings_SubBatchesAndPreservesOrder(t *testing.T) {
	resetTEIBatchSizeCache()
	// /info says 32; the engine 413s anything over 32. 40 inputs must still
	// succeed via two batches (32 + 8), concatenated in input order.
	st := newBatchTEIStub(t, "present", 32, 32)

	const n = 40
	res, err := callTEIEmbeddings(st.srv.URL, &EmbeddingRequest{Model: "gte", Input: numberedInputs(n)})
	if err != nil {
		t.Fatalf("expected sub-batched success, got error: %v", err)
	}
	if len(res.Embeddings) != n {
		t.Fatalf("got %d embeddings, want %d", len(res.Embeddings), n)
	}
	for i := range res.Embeddings {
		if len(res.Embeddings[i]) != 1 || res.Embeddings[i][0] != float64(i) {
			t.Fatalf("embedding[%d] = %v, want [%d] (order not preserved across batches)", i, res.Embeddings[i], i)
		}
	}
	// Usage summed across batches (40 inputs => 40 prompt tokens).
	if res.Usage.PromptTokens != n || res.Usage.TotalTokens != n {
		t.Errorf("usage = %+v, want prompt=%d total=%d", res.Usage, n, n)
	}
	// The limit was actually applied: >1 request, none exceeding 32.
	sizes := st.sizes()
	if len(sizes) < 2 {
		t.Fatalf("expected >=2 batched requests, got %d (sizes=%v)", len(sizes), sizes)
	}
	for _, s := range sizes {
		if s > 32 {
			t.Fatalf("batch of %d exceeded the limit of 32 (sizes=%v)", s, sizes)
		}
	}
}

func TestCallTEIEmbeddings_InfoLimitResolution(t *testing.T) {
	cases := []struct {
		name      string
		infoMode  string
		infoLimit int
		maxBatch  int // engine rejects batches above this
		wantMax   int // every observed request must be <= this
	}{
		{name: "info-driven limit of 8", infoMode: "present", infoLimit: 8, maxBatch: 8, wantMax: 8},
		{name: "fallback 32 when /info missing", infoMode: "missing", maxBatch: 32, wantMax: 32},
		{name: "fallback 32 when /info omits field", infoMode: "absent", maxBatch: 32, wantMax: 32},
		{name: "fallback 32 when field is zero", infoMode: "zero", maxBatch: 32, wantMax: 32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetTEIBatchSizeCache()
			st := newBatchTEIStub(t, tc.infoMode, tc.infoLimit, tc.maxBatch)

			const n = 40
			res, err := callTEIEmbeddings(st.srv.URL, &EmbeddingRequest{Model: "gte", Input: numberedInputs(n)})
			if err != nil {
				t.Fatalf("expected success, got error: %v", err)
			}
			if len(res.Embeddings) != n {
				t.Fatalf("got %d embeddings, want %d", len(res.Embeddings), n)
			}
			for i := range res.Embeddings {
				if res.Embeddings[i][0] != float64(i) {
					t.Fatalf("embedding[%d][0] = %v, want %d", i, res.Embeddings[i][0], i)
				}
			}
			for _, s := range st.sizes() {
				if s > tc.wantMax {
					t.Fatalf("batch of %d exceeded resolved limit %d (sizes=%v)", s, tc.wantMax, st.sizes())
				}
			}
		})
	}
}

func TestCallTEIEmbeddings_ShortResponseErrors(t *testing.T) {
	resetTEIBatchSizeCache()
	// Returns only one embedding for two inputs; must error, not store a nil.
	srv := newTEIServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"index":0,"embedding":[0.1]}],"model":"gte","usage":{"prompt_tokens":1,"total_tokens":1}}`)
	})
	defer srv.Close()

	if _, err := callTEIEmbeddings(srv.URL, &EmbeddingRequest{Model: "gte", Input: []string{"a", "b"}}); err == nil {
		t.Fatalf("expected error on short response, got nil")
	}
}

func TestEmbeddingHandler_Execute_SubBatchesLargeInput(t *testing.T) {
	resetTEIBatchSizeCache()
	// The EMBEDDING job takes an unbounded `input` array from the caller
	// (run_fabric_embeddings). 33 inputs against a 32-cap engine must succeed via
	// the shared sub-batching path, with usage summed across batches.
	st := newBatchTEIStub(t, "present", 32, 32)
	t.Setenv("CITADEL_TEI_URL", st.srv.URL)

	const n = 33
	raw, _ := json.Marshal(numberedInputs(n))
	h := &EmbeddingHandler{}
	job := &nexus.Job{
		ID:      "emb-big",
		Type:    "embedding",
		Payload: map[string]string{"model": "gte", "input": string(raw)},
	}
	out, err := h.Execute(JobContext{}, job)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	var res EmbeddingResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not valid JSON: %v", err)
	}
	if len(res.Embeddings) != n {
		t.Fatalf("got %d embeddings, want %d", len(res.Embeddings), n)
	}
	for i := range res.Embeddings {
		if res.Embeddings[i][0] != float64(i) {
			t.Fatalf("embedding[%d][0] = %v, want %d", i, res.Embeddings[i][0], i)
		}
	}
	if res.Usage.PromptTokens != n {
		t.Errorf("usage prompt_tokens = %d, want %d (summed across batches)", res.Usage.PromptTokens, n)
	}
	if len(st.sizes()) < 2 {
		t.Fatalf("expected >=2 batched requests, got %v", st.sizes())
	}
}

func TestFileIndex_LargeFileSubBatches(t *testing.T) {
	resetTEIBatchSizeCache()
	// A single file yielding >32 chunks must index successfully against a 32-cap
	// engine (the #1260 regression: it used to abort the whole walk with a 413).
	st := newBatchTEIStub(t, "present", 32, 32)
	t.Setenv("CITADEL_TEI_URL", st.srv.URL)
	t.Setenv("CITADEL_INDEX_DB", "")

	ws := t.TempDir()
	// 40 paragraphs of ~800 bytes each: each is under chunkTargetBytes (1000) but
	// two together exceed it, so chunkText yields exactly one chunk per paragraph.
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString(strings.Repeat("x", 800))
		b.WriteString("\n\n")
	}
	if err := os.WriteFile(filepath.Join(ws, "big.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "index.db")
	idx := NewFileIndexHandler(ws, dbPath)
	out, err := idx.Execute(JobContext{}, &nexus.Job{ID: "j", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
	if err != nil {
		t.Fatalf("FILE_INDEX aborted on a large file: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal index result: %v", err)
	}
	if res["files_indexed"].(float64) != 1 {
		t.Fatalf("expected 1 file indexed, got %v (result=%s)", res["files_indexed"], out)
	}
	if res["files_failed"].(float64) != 0 {
		t.Fatalf("expected 0 files failed, got %v (result=%s)", res["files_failed"], out)
	}
	if res["chunks_upserted"].(float64) < 33 {
		t.Fatalf("expected >=33 chunks upserted, got %v (result=%s)", res["chunks_upserted"], out)
	}
	sizes := st.sizes()
	if len(sizes) < 2 {
		t.Fatalf("expected the large file to be split into >=2 batches, got %v", sizes)
	}
	for _, s := range sizes {
		if s > 32 {
			t.Fatalf("a batch of %d exceeded the TEI limit (sizes=%v)", s, sizes)
		}
	}
}

func TestFileIndex_ContinuesPastFailedFile(t *testing.T) {
	resetTEIBatchSizeCache()
	// TEI 500s on any input containing the poison marker; one poisoned file must
	// be recorded as failed and the walk must continue to index the others.
	const poison = "POISONPILL"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/health") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/info") {
			fmt.Fprint(w, `{"model_id":"gte","max_client_batch_size":32}`)
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, in := range req.Input {
			if strings.Contains(in, poison) {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"error":"boom"}`)
				return
			}
		}
		type dataItem struct {
			Embedding []float64 `json:"embedding"`
			Index     int       `json:"index"`
		}
		data := make([]dataItem, len(req.Input))
		for i := range req.Input {
			data[i] = dataItem{Embedding: []float64{0.1, 0.2, 0.3}, Index: i}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": "gte", "data": data,
			"usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1}})
	}))
	defer srv.Close()
	t.Setenv("CITADEL_TEI_URL", srv.URL)
	t.Setenv("CITADEL_INDEX_DB", "")

	ws := t.TempDir()
	// Lexical walk order: a.txt, b_poison.txt, c.txt.
	for name, body := range map[string]string{
		"a.txt":        "alpha content is fine",
		"b_poison.txt": poison + " this file will fail to embed",
		"c.txt":        "charlie content is fine",
	} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dbPath := filepath.Join(t.TempDir(), "index.db")
	idx := NewFileIndexHandler(ws, dbPath)
	out, err := idx.Execute(JobContext{}, &nexus.Job{ID: "j", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
	if err != nil {
		t.Fatalf("one bad file must not abort the walk, got error: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal index result: %v", err)
	}
	if res["files_indexed"].(float64) != 2 {
		t.Fatalf("expected 2 files indexed (the two good ones), got %v (result=%s)", res["files_indexed"], out)
	}
	if res["files_failed"].(float64) != 1 {
		t.Fatalf("expected 1 file failed, got %v (result=%s)", res["files_failed"], out)
	}
}
