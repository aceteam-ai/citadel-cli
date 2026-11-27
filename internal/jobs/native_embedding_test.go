package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func nativeEmbeddingJob(model, dimensions string) *nexus.Job {
	return &nexus.Job{ID: "embedding-fixture", Type: "embedding", Payload: map[string]string{
		"model": model, "dimensions": dimensions, "input": `["synthetic one","synthetic two"]`,
	}}
}

func TestNativeEmbeddingConstructorRefusesUnboundOrNonlocal(t *testing.T) {
	for _, endpoint := range []string{"", "http://example.invalid:8102", "http://localhost:8102", "http://0.0.0.0:8102", "https://127.0.0.1:8102", "http://fixture-secret@127.0.0.1:8102", "http://127.0.0.1:8102/path", "http://127.0.0.1:8102?fixture-secret", "http://127.0.0.1:8102#"} {
		if h, err := NewNativeEmbeddingHandler(endpoint, "fixture-model", 2); err == nil || h != nil || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatalf("endpoint accepted or reflected: handler=%v err=%v", h, err)
		}
	}
	for _, model := range []string{"", " ", "fixture\nmodel", "model,alias", " model"} {
		if _, err := NewNativeEmbeddingHandler("http://127.0.0.1:8102", model, 2); err == nil {
			t.Fatal("invalid model accepted")
		}
	}
	for _, dim := range []int{-1, 0} {
		if _, err := NewNativeEmbeddingHandler("http://127.0.0.1:8102", "fixture-model", dim); err == nil {
			t.Fatal("invalid dimension accepted")
		}
	}
}

func TestNativeEmbeddingRequestMismatchHasNoIO(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer s.Close()
	h, err := NewNativeEmbeddingHandler(s.URL, "fixture-model", 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range []*nexus.Job{nativeEmbeddingJob("other-model", "2"), nativeEmbeddingJob("fixture-model", "3"), nativeEmbeddingJob("fixture-model", "0"), nativeEmbeddingJob("fixture-model", "")} {
		if out, err := h.Execute(JobContext{}, job); err == nil || out != nil {
			t.Fatalf("mismatch accepted: %s %v", out, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("mismatch contacted a service")
	}
	h.Dispatched = true
	if _, err := h.Execute(JobContext{}, nativeEmbeddingJob("fixture-model", "2")); !errors.Is(err, ErrReceiptSigningUnavailable) {
		t.Fatalf("missing signer: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("unsigned dispatch contacted a service")
	}
}

func TestNativeEmbeddingSignedResultAndModelBinding(t *testing.T) {
	for _, tc := range []struct {
		name, responseModel string
		dim                 int
		ok                  bool
	}{
		{"matching", "fixture-model", 2, true},
		{"different model", "other-model", 2, false},
		{"missing model", "", 2, false},
		{"different dimensions", "fixture-model", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTEIServer(t, func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Model      string `json:"model"`
					Dimensions int    `json:"dimensions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.Model != "fixture-model" || request.Dimensions != 2 {
					t.Error("request lost model binding")
				}
				vectors := []map[string]any{{"index": 1, "embedding": make([]float64, tc.dim)}, {"index": 0, "embedding": make([]float64, tc.dim)}}
				_ = json.NewEncoder(w).Encode(map[string]any{"model": tc.responseModel, "data": vectors, "usage": map[string]int{"prompt_tokens": 4, "total_tokens": 4}})
			})
			defer s.Close()
			h, err := NewNativeEmbeddingHandler(s.URL, "fixture-model", 2)
			if err != nil {
				t.Fatal(err)
			}
			h.Signer, h.Dispatched, h.Origin = newInMemSigner(t), true, OriginDispatched
			out, err := h.Execute(JobContext{}, nativeEmbeddingJob("fixture-model", "2"))
			if !tc.ok {
				if err == nil || out != nil {
					t.Fatalf("response mismatch accepted: %s %v", out, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			result := decodeOut(t, out)
			if result["model"] != "fixture-model" || result["dimensions"] != float64(2) {
				t.Fatalf("wrong identity: %v", result)
			}
			receipt := firstWorkReceipt(t, result)
			if receipt["action"] != WorkActionEmbedServed || receipt["signature"] == "" {
				t.Fatal("missing signed embedding receipt")
			}
			expected, err := manifestSHA256(embedInputManifest{Action: WorkActionEmbedServed, Model: "fixture-model", Dimensions: 2, Inputs: hashEmbeddingInputs([]string{"synthetic one", "synthetic two"})})
			if err != nil || receipt["input_sha256"] != expected {
				t.Fatal("receipt does not bind exact model and dimensions")
			}
			expectedOutput, err := manifestSHA256(embedOutputManifest{Action: WorkActionEmbedServed, Model: "fixture-model", Count: 2, Dim: 2, PromptTokens: 4, TotalTokens: 4})
			if err != nil || receipt["output_sha256"] != expectedOutput {
				t.Fatal("receipt output does not bind the served model and dimensions")
			}
		})
	}
}

type nativeEmbeddingCountingSigner struct {
	*inMemSigner
	calls int
}

func (s *nativeEmbeddingCountingSigner) Sign(payload []byte) ([]byte, error) {
	s.calls++
	return s.inMemSigner.Sign(payload)
}

func TestNativeEmbeddingLaterBatchMismatchEmitsNoPartialReceipt(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"max_client_batch_size":1}`)) })
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, _ *http.Request) {
		model := "fixture-model"
		if calls.Add(1) == 2 {
			model = "different-model"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": model, "data": []map[string]any{{"index": 0, "embedding": []float64{0.1, 0.2}}}})
	})
	s := httptest.NewServer(mux)
	defer s.Close()
	h, err := NewNativeEmbeddingHandler(s.URL, "fixture-model", 2)
	if err != nil {
		t.Fatal(err)
	}
	signer := &nativeEmbeddingCountingSigner{inMemSigner: newInMemSigner(t)}
	h.Signer, h.Dispatched = signer, true
	if out, err := h.Execute(JobContext{}, nativeEmbeddingJob("fixture-model", "2")); err == nil || out != nil {
		t.Fatal("partial operation emitted output")
	}
	if calls.Load() != 2 || signer.calls != 0 {
		t.Fatalf("batch calls=%d receipt signatures=%d", calls.Load(), signer.calls)
	}
}

func TestNativeEmbeddingEndpointIgnoresTEIOverrideAndRefusesRedirect(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { foreignCalls.Add(1); w.WriteHeader(200) }))
	defer foreign.Close()
	t.Setenv("CITADEL_TEI_URL", foreign.URL)
	local := newTEIServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
	})
	defer local.Close()
	h, err := NewNativeEmbeddingHandler(local.URL, "fixture-model", 2)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := h.Execute(JobContext{}, nativeEmbeddingJob("fixture-model", "2")); err == nil || out != nil {
		t.Fatal("redirect accepted")
	}
	if foreignCalls.Load() != 0 {
		t.Fatal("followed redirect or TEI override")
	}
}

func TestNativeEmbeddingCancellationRemainsHard(t *testing.T) {
	s := newTEIServer(t, func(w http.ResponseWriter, _ *http.Request) { t.Error("cancelled operation sent input") })
	defer s.Close()
	h, err := NewNativeEmbeddingHandler(s.URL, "fixture-model", 2)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := h.Execute(JobContext{Ctx: ctx}, nativeEmbeddingJob("fixture-model", "2")); !errors.Is(err, context.Canceled) || out != nil {
		t.Fatalf("cancellation: %s %v", out, err)
	}
}
