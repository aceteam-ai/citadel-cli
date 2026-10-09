package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// newTEIServer returns a stub TEI server. health controls whether /health is OK;
// the /v1/embeddings handler returns the supplied response JSON with status 200,
// or, if respFn is set, delegates to it.
func newTEIServer(t *testing.T, respFn func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/embeddings", respFn)
	return httptest.NewServer(mux)
}

func TestParseEmbeddingPayload(t *testing.T) {
	tests := []struct {
		name      string
		payload   map[string]string
		wantInput []string
		wantDim   int
		wantErr   bool
	}{
		{
			name:      "json array input",
			payload:   map[string]string{"model": "gte", "input": `["hello","world"]`},
			wantInput: []string{"hello", "world"},
		},
		{
			name:      "scalar string fallback",
			payload:   map[string]string{"model": "gte", "input": "just one"},
			wantInput: []string{"just one"},
		},
		{
			name:      "input with spaces preserved via json",
			payload:   map[string]string{"model": "gte", "input": `["a b c","d e"]`},
			wantInput: []string{"a b c", "d e"},
		},
		{
			name:      "dimensions parsed",
			payload:   map[string]string{"model": "gte", "input": `["x"]`, "dimensions": "256"},
			wantInput: []string{"x"},
			wantDim:   256,
		},
		{
			name:    "missing model",
			payload: map[string]string{"input": `["x"]`},
			wantErr: true,
		},
		{
			name:    "missing input",
			payload: map[string]string{"model": "gte"},
			wantErr: true,
		},
		{
			name:    "bad dimensions",
			payload: map[string]string{"model": "gte", "input": `["x"]`, "dimensions": "notanint"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := parseEmbeddingPayload(tt.payload)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(req.Input, tt.wantInput) {
				t.Errorf("input = %v, want %v", req.Input, tt.wantInput)
			}
			if req.Dimensions != tt.wantDim {
				t.Errorf("dimensions = %d, want %d", req.Dimensions, tt.wantDim)
			}
		})
	}
}

func TestCallTEIEmbeddings_Success(t *testing.T) {
	srv := newTEIServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify request shape.
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("bad request body: %v", err)
		}
		if req["model"] != "gte" {
			t.Errorf("model = %v, want gte", req["model"])
		}
		// Return two vectors, intentionally out of index order to verify
		// the handler reorders by index.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"object":"list",
			"data":[
				{"object":"embedding","index":1,"embedding":[0.3,0.4]},
				{"object":"embedding","index":0,"embedding":[0.1,0.2]}
			],
			"model":"gte-multilingual-base",
			"usage":{"prompt_tokens":5,"total_tokens":5}
		}`))
	})
	defer srv.Close()

	res, err := callTEIEmbeddings(srv.URL, &EmbeddingRequest{
		Model: "gte",
		Input: []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := [][]float64{{0.1, 0.2}, {0.3, 0.4}}
	if !reflect.DeepEqual(res.Embeddings, want) {
		t.Errorf("embeddings = %v, want %v (must be index-ordered)", res.Embeddings, want)
	}
	if res.Dimensions != 2 {
		t.Errorf("dimensions = %d, want 2", res.Dimensions)
	}
	if res.Model != "gte-multilingual-base" {
		t.Errorf("model = %q, want gte-multilingual-base", res.Model)
	}
	if res.Usage.PromptTokens != 5 || res.Usage.TotalTokens != 5 {
		t.Errorf("usage = %+v, want prompt=5 total=5", res.Usage)
	}
}

func TestCallTEIEmbeddings_ForwardsDimensions(t *testing.T) {
	var gotDimensions any
	var dimensionsPresent bool
	srv := newTEIServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		gotDimensions, dimensionsPresent = req["dimensions"]
		dim := 1
		if dimensionsPresent {
			dim = int(gotDimensions.(float64))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  []map[string]any{{"index": 0, "embedding": make([]float64, dim)}},
			"model": "gte", "usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1},
		})
	})
	defer srv.Close()

	// With dimensions set, it must be forwarded.
	if _, err := callTEIEmbeddings(srv.URL, &EmbeddingRequest{Model: "gte", Input: []string{"x"}, Dimensions: 128}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dimensionsPresent {
		t.Fatalf("expected dimensions forwarded to TEI")
	}
	if int(gotDimensions.(float64)) != 128 {
		t.Errorf("dimensions = %v, want 128", gotDimensions)
	}

	// Without dimensions, it must be omitted (native dims).
	if _, err := callTEIEmbeddings(srv.URL, &EmbeddingRequest{Model: "gte", Input: []string{"x"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dimensionsPresent {
		t.Errorf("dimensions should be omitted when not requested")
	}
}

func TestCallTEIEmbeddings_UpstreamError(t *testing.T) {
	srv := newTEIServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"model not loaded"}`))
	})
	defer srv.Close()

	_, err := callTEIEmbeddings(srv.URL, &EmbeddingRequest{Model: "gte", Input: []string{"x"}})
	if err == nil {
		t.Fatalf("expected error on 500, got nil")
	}
}

func TestEmbeddingHandler_Execute(t *testing.T) {
	srv := newTEIServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":0,"embedding":[0.5,0.6,0.7]}],"model":"gte","usage":{"prompt_tokens":2,"total_tokens":2}}`))
	})
	defer srv.Close()

	t.Setenv("CITADEL_TEI_URL", srv.URL)

	h := &EmbeddingHandler{}
	job := &nexus.Job{
		ID:   "job-1",
		Type: "embedding",
		Payload: map[string]string{
			"model": "gte",
			"input": `["hello"]`,
		},
	}
	out, err := h.Execute(JobContext{}, job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res EmbeddingResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not valid JSON: %v", err)
	}
	if len(res.Embeddings) != 1 || res.Dimensions != 3 {
		t.Errorf("got %d embeddings dim=%d, want 1 dim=3", len(res.Embeddings), res.Dimensions)
	}
}

func TestEmbeddingHandler_Execute_BadPayload(t *testing.T) {
	h := &EmbeddingHandler{}
	job := &nexus.Job{ID: "job-2", Type: "embedding", Payload: map[string]string{"input": `["x"]`}}
	if _, err := h.Execute(JobContext{}, job); err == nil {
		t.Fatalf("expected error for missing model")
	}
}

func TestTEIEmbeddingOperation_SubBatchesUsingInfoAndPreservesOrder(t *testing.T) {
	var infoCalls int
	var batchSizes []int
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		infoCalls++
		_ = json.NewEncoder(w).Encode(map[string]int{"max_client_batch_size": 32})
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model      string   `json:"model"`
			Input      []string `json:"input"`
			Dimensions int      `json:"dimensions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(req.Input) > 32 {
			http.Error(w, "batch too large", http.StatusRequestEntityTooLarge)
			return
		}
		if req.Dimensions != 2 {
			t.Errorf("dimensions = %d, want 2 on every sub-batch", req.Dimensions)
		}
		batchSizes = append(batchSizes, len(req.Input))
		data := make([]map[string]any, 0, len(req.Input))
		// Reverse response order. Explicit indices must restore input order
		// within each batch before the operation appends it globally.
		for i := len(req.Input) - 1; i >= 0; i-- {
			n, err := strconv.Atoi(strings.TrimPrefix(req.Input[i], "item-"))
			if err != nil {
				t.Fatalf("parse input %q: %v", req.Input[i], err)
			}
			data = append(data, map[string]any{"index": i, "embedding": []float64{float64(n), float64(n + 1)}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": data, "model": "Alibaba-NLP/gte-multilingual-base",
			"usage": map[string]int{"prompt_tokens": len(req.Input), "total_tokens": len(req.Input)},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	inputs := make([]string, 200)
	for i := range inputs {
		inputs[i] = fmt.Sprintf("item-%d", i)
	}
	res, err := newTEIEmbeddingOperation(srv.URL).embed(context.Background(), &EmbeddingRequest{
		Model: "gte", Input: inputs, Dimensions: 2,
	})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if infoCalls != 1 {
		t.Fatalf("GET /info calls = %d, want exactly 1", infoCalls)
	}
	if want := []int{32, 32, 32, 32, 32, 32, 8}; !reflect.DeepEqual(batchSizes, want) {
		t.Fatalf("batch sizes = %v, want %v", batchSizes, want)
	}
	if len(res.Embeddings) != len(inputs) || res.Usage.TotalTokens != len(inputs) {
		t.Fatalf("result cardinality/usage = %d/%d, want %d/%d", len(res.Embeddings), res.Usage.TotalTokens, len(inputs), len(inputs))
	}
	if res.Model != "Alibaba-NLP/gte-multilingual-base" {
		t.Fatalf("response model = %q, want TEI's stable full model ID", res.Model)
	}
	for i, vec := range res.Embeddings {
		want := []float64{float64(i), float64(i + 1)}
		if !reflect.DeepEqual(vec, want) {
			t.Fatalf("embedding %d = %v, want %v", i, vec, want)
		}
	}
}

func TestResolveTEIClientBatchSizeFallbackAndBounds(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{name: "valid", status: http.StatusOK, body: `{"max_client_batch_size":17}`, want: 17},
		{name: "missing", status: http.StatusOK, body: `{}`, want: teiDefaultClientBatchSize},
		{name: "zero", status: http.StatusOK, body: `{"max_client_batch_size":0}`, want: teiDefaultClientBatchSize},
		{name: "negative", status: http.StatusOK, body: `{"max_client_batch_size":-1}`, want: teiDefaultClientBatchSize},
		{name: "too large", status: http.StatusOK, body: `{"max_client_batch_size":1025}`, want: teiDefaultClientBatchSize},
		{name: "malformed", status: http.StatusOK, body: `{`, want: teiDefaultClientBatchSize},
		{name: "trailing json", status: http.StatusOK, body: `{"max_client_batch_size":17}{}`, want: teiDefaultClientBatchSize},
		{name: "oversized", status: http.StatusOK, body: strings.Repeat(" ", (1<<20)+1), want: teiDefaultClientBatchSize},
		{name: "not found", status: http.StatusNotFound, body: `missing`, want: teiDefaultClientBatchSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			got, err := resolveTEIClientBatchSize(context.Background(), srv.Client(), srv.URL)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got != tt.want {
				t.Fatalf("batch size = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveTEIClientBatchSizePropagatesParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := resolveTEIClientBatchSize(ctx, http.DefaultClient, "http://127.0.0.1:1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestResolveTEIClientBatchSizePropagatesInflightBodyCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"max_client_batch_size":`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := resolveTEIClientBatchSize(ctx, srv.Client(), srv.URL)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for /info response body")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancelled /info request")
	}
}

func TestWaitForTEIReadyBoundsStalledHealthRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	start := time.Now()
	err := waitForTEIReady(context.Background(), srv.Client(), srv.URL, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected readiness timeout")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stalled health request never reached server")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("stalled health request exceeded ready budget: %v", elapsed)
	}
}

func TestTEIEmbeddingOperationPropagatesInflightParentCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"max_client_batch_size":32}`)
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	srv := httptest.NewServer(mux)
	defer func() {
		close(release)
		srv.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		result *EmbeddingResult
		err    error
	}, 1)
	go func() {
		result, err := newTEIEmbeddingOperation(srv.URL).embed(ctx, &EmbeddingRequest{Model: "gte", Input: []string{"x"}})
		done <- struct {
			result *EmbeddingResult
			err    error
		}{result, err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("embedding request never reached server")
	}
	cancel()
	select {
	case got := <-done:
		if got.result != nil || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("result/error = %+v/%v, want nil/context.Canceled", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancelled embedding request")
	}
}

func TestTEIEmbeddingOperationUsesAdvertisedSmallLimitAndFallback(t *testing.T) {
	tests := []struct {
		name      string
		infoCode  int
		infoBody  string
		inputs    int
		wantSizes []int
	}{
		{name: "advertised seven", infoCode: http.StatusOK, infoBody: `{"max_client_batch_size":7}`, inputs: 20, wantSizes: []int{7, 7, 6}},
		{name: "unavailable falls back thirty two", infoCode: http.StatusServiceUnavailable, inputs: 33, wantSizes: []int{32, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var infoCalls int
			var sizes []int
			mux := http.NewServeMux()
			mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
				infoCalls++
				w.WriteHeader(tt.infoCode)
				_, _ = io.WriteString(w, tt.infoBody)
			})
			mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Input []string `json:"input"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				sizes = append(sizes, len(req.Input))
				data := make([]map[string]any, len(req.Input))
				for i := range data {
					data[i] = map[string]any{"index": i, "embedding": []float64{1}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "gte"})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			inputs := make([]string, tt.inputs)
			if _, err := newTEIEmbeddingOperation(srv.URL).embed(context.Background(), &EmbeddingRequest{Model: "gte", Input: inputs}); err != nil {
				t.Fatalf("embed: %v", err)
			}
			if infoCalls != 1 || !reflect.DeepEqual(sizes, tt.wantSizes) {
				t.Fatalf("info calls/batches = %d/%v, want 1/%v", infoCalls, sizes, tt.wantSizes)
			}
		})
	}
}

func TestTEIEmbeddingOperationRejectsInvalidOrPartialBatchesAtomically(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		dimensions int
	}{
		{name: "cardinality", mode: "cardinality"},
		{name: "duplicate index", mode: "duplicate"},
		{name: "empty vector", mode: "empty"},
		{name: "within batch dimensions", mode: "within-dim"},
		{name: "across batch dimensions", mode: "across-dim"},
		{name: "model changes after requested model", mode: "model"},
		{name: "requested dimensions", mode: "requested-dim", dimensions: 3},
		{name: "late http failure", mode: "late-http"},
		{name: "truncated response", mode: "truncated"},
		{name: "oversized response", mode: "oversized"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			mux := http.NewServeMux()
			mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"max_client_batch_size":2}`)
			})
			mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
				calls++
				var req struct {
					Input []string `json:"input"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				if tt.mode == "late-http" && calls == 2 {
					http.Error(w, "SECRET-INPUT-ECHO", http.StatusInternalServerError)
					return
				}
				if tt.mode == "truncated" {
					_, _ = io.WriteString(w, `{"data":[`)
					return
				}
				if tt.mode == "oversized" {
					_, _ = io.WriteString(w, strings.Repeat(" ", teiEmbeddingResponseLimit+1))
					return
				}
				data := []map[string]any{
					{"index": 0, "embedding": []float64{1, 0}},
					{"index": 1, "embedding": []float64{0, 1}},
				}
				if len(req.Input) == 1 {
					data = data[:1]
				}
				switch {
				case tt.mode == "cardinality" && calls == 2:
					data = data[:1]
				case tt.mode == "duplicate":
					data[1]["index"] = 0
				case tt.mode == "empty":
					data[0]["embedding"] = []float64{}
				case tt.mode == "within-dim":
					data[1]["embedding"] = []float64{0, 1, 2}
				case tt.mode == "across-dim" && calls == 2:
					data[0]["embedding"] = []float64{1, 0, 0}
					data[1]["embedding"] = []float64{0, 1, 0}
				}
				model := "gte"
				if tt.mode == "model" && calls == 2 {
					model = "SECRET-MODEL-ECHO"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": model})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			res, err := newTEIEmbeddingOperation(srv.URL).embed(context.Background(), &EmbeddingRequest{
				Model: "gte", Input: []string{"a", "b", "c", "d"}, Dimensions: tt.dimensions,
			})
			if err == nil {
				t.Fatalf("expected validation error, got result %+v", res)
			}
			if res != nil {
				t.Fatalf("late failure exposed partial result: %+v", res)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("upstream response body leaked through error: %v", err)
			}
		})
	}
}

func TestTEIEmbeddingOperationRediscoversLimitBetweenOperations(t *testing.T) {
	var mu sync.Mutex
	limit := 32
	infoCalls := 0
	var sizes []int
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		infoCalls++
		_ = json.NewEncoder(w).Encode(map[string]int{"max_client_batch_size": limit})
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad owned fixture request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		sizes = append(sizes, len(req.Input))
		tooLarge := len(req.Input) > limit
		mu.Unlock()
		if tooLarge {
			http.Error(w, "batch exceeds current limit", http.StatusRequestEntityTooLarge)
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
	req := &EmbeddingRequest{Model: "gte", Input: numberedInputs(33)}
	if _, err := newTEIEmbeddingOperation(srv.URL).embed(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	limit = 8 // a restart or reconfiguration at the same service URL
	sizes = nil
	mu.Unlock()
	if _, err := newTEIEmbeddingOperation(srv.URL).embed(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if infoCalls != 2 || !reflect.DeepEqual(sizes, []int{8, 8, 8, 8, 1}) {
		t.Fatalf("info calls/new-operation sizes = %d/%v, want 2/[8 8 8 8 1]", infoCalls, sizes)
	}
}

func TestEmbeddingHandlerExecuteSubBatchesActualJobPath(t *testing.T) {
	var infoCalls, embedCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		infoCalls++
		_, _ = io.WriteString(w, `{"max_client_batch_size":32}`)
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		embedCalls++
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) > 32 {
			http.Error(w, "too many", http.StatusRequestEntityTooLarge)
			return
		}
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float64{float64(i), 1}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "gte"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("CITADEL_TEI_URL", srv.URL)

	inputs := make([]string, 65)
	for i := range inputs {
		inputs[i] = fmt.Sprintf("input-%d", i)
	}
	rawInputs, _ := json.Marshal(inputs)
	out, err := (&EmbeddingHandler{}).Execute(JobContext{}, &nexus.Job{ID: "batch-job", Type: "embedding", Payload: map[string]string{
		"model": "gte", "input": string(rawInputs),
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var res EmbeddingResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(res.Embeddings) != 65 || infoCalls != 1 || embedCalls != 3 {
		t.Fatalf("embeddings/info/posts = %d/%d/%d, want 65/1/3", len(res.Embeddings), infoCalls, embedCalls)
	}
}

func TestEmbeddingHandlerDoesNotEchoInconsistentUpstreamModel(t *testing.T) {
	const secret = "SECRET-MODEL-ECHO"
	posts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"max_client_batch_size":2}`)
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		posts++
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float64{1}}
		}
		model := "gte"
		if posts == 2 {
			model = secret
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": model})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("CITADEL_TEI_URL", srv.URL)

	_, err := (&EmbeddingHandler{}).Execute(JobContext{}, &nexus.Job{ID: "no-echo", Type: "embedding", Payload: map[string]string{
		"model": "gte", "input": `["one","two","three"]`,
	}})
	if err == nil {
		t.Fatal("expected inconsistent model error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("upstream model leaked through EMBEDDING error: %v", err)
	}
}

func TestEmbedTextsRejectsFloat32Overflow(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"max_client_batch_size":32}`)
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1e100]}],"model":"gte"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	vecs, err := embedTexts(context.Background(), newTEIEmbeddingOperation(srv.URL), "gte", []string{"x"})
	if err == nil || vecs != nil {
		t.Fatalf("overflow result/error = %v/%v, want nil/error", vecs, err)
	}
}
