// internal/jobs/vllm_inference_test.go
package jobs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/services"
)

// TestVLLMInferenceBaseURLUsesRegistryPort pins the citadel#428 fix: the
// handler must resolve vLLM's citadel-owned HOST port (services.VLLMHostPort)
// rather than the vLLM container's in-container port 8000, which was never a
// reachable host port on any published-port setup.
func TestVLLMInferenceBaseURLUsesRegistryPort(t *testing.T) {
	got := vllmInferenceBaseURL()
	want := fmt.Sprintf("http://localhost:%d", services.VLLMHostPort)
	if got != want {
		t.Fatalf("vllmInferenceBaseURL() = %q, want %q", got, want)
	}
	if got == "http://localhost:8000" {
		t.Fatalf("vllmInferenceBaseURL() must not resolve to the stale in-container port 8000")
	}
}

// TestVLLMInferenceHandlerExecute verifies the handler hits /health then
// /v1/completions against whatever base URL vllmInferenceBaseURL() resolves
// to, and returns the completion text. This exercises the full Execute path
// (health check + inference request) against a mock vLLM server, standing in
// for the real citadel-owned host port.
func TestVLLMInferenceHandlerExecute(t *testing.T) {
	var sawHealth, sawCompletions bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			sawHealth = true
			w.WriteHeader(http.StatusOK)
		case "/v1/completions":
			sawCompletions = true
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			if body["model"] != "test-model" {
				t.Errorf("request model = %v, want test-model", body["model"])
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{"text": "  hello from vllm  "},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	orig := vllmInferenceBaseURL
	vllmInferenceBaseURL = func() string { return server.URL }
	defer func() { vllmInferenceBaseURL = orig }()

	h := &VLLMInferenceHandler{}
	job := &nexus.Job{
		ID:   "job-1",
		Type: "VLLM_INFERENCE",
		Payload: map[string]string{
			"model":  "test-model",
			"prompt": "hi",
		},
	}

	out, err := h.Execute(JobContext{}, job)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(out) != "hello from vllm" {
		t.Fatalf("Execute() output = %q, want %q", out, "hello from vllm")
	}
	if !sawHealth {
		t.Errorf("handler never hit /health")
	}
	if !sawCompletions {
		t.Errorf("handler never hit /v1/completions")
	}
}

// TestVLLMInferenceHandlerExecuteMissingFields verifies the payload validation
// still short-circuits before any network call.
func TestVLLMInferenceHandlerExecuteMissingFields(t *testing.T) {
	h := &VLLMInferenceHandler{}
	job := &nexus.Job{
		ID:      "job-2",
		Type:    "VLLM_INFERENCE",
		Payload: map[string]string{"model": "test-model"}, // missing "prompt"
	}
	if _, err := h.Execute(JobContext{}, job); err == nil {
		t.Fatal("Execute() expected error for missing prompt field, got nil")
	}
}
