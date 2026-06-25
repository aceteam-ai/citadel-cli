package jobs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

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
		w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1]}],"model":"gte","usage":{"prompt_tokens":1,"total_tokens":1}}`))
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
