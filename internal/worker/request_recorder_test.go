package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLLMInferenceHandler_RecordsBackendOnDispatch verifies the node-routed
// request recorder (citadel #691) is called with the resolved backend name
// exactly once, after the engine passes its readiness probe and before the
// request is dispatched -- the hook that gives ollama/bonsai/llamacpp/
// unlimited-ocr (none of which expose a scrapeable request metric) a real
// last_request_at in the heartbeat instead of "never".
func TestLLMInferenceHandler_RecordsBackendOnDispatch(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	ts := newChatCompletionsServer(t, frames, nil)
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL

	var recorded []string
	h.WithRequestRecorder(func(engine string) { recorded = append(recorded, engine) })

	job := &Job{
		ID:   "job-record",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":    "bonsai-27b",
			"backend":  "bonsai",
			"stream":   true,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		},
	}
	stream := &MockStreamWriter{}
	if _, err := h.Execute(context.Background(), job, stream); err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if len(recorded) != 1 || recorded[0] != "bonsai" {
		t.Fatalf("expected exactly one record for %q, got %v", "bonsai", recorded)
	}
}

// TestLLMInferenceHandler_DoesNotRecordOnWarmingEngine verifies a request that
// never clears the readiness gate (the engine is still warming) is NOT
// recorded: it never actually reached the engine, so recording it would
// fabricate activity that didn't happen.
func TestLLMInferenceHandler_DoesNotRecordOnWarmingEngine(t *testing.T) {
	// A server that never answers the readiness probe successfully (always
	// 503) simulates an engine still loading weights.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL

	called := false
	h.WithRequestRecorder(func(string) { called = true })

	job := &Job{
		ID:   "job-warming",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":    "bonsai-27b",
			"backend":  "bonsai",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		},
	}
	stream := &MockStreamWriter{}
	// Not asserting the exact warming/failure shape here -- llm_readiness_test.go
	// already covers that contract -- only that dispatch (and therefore
	// recording) never happened.
	if _, err := h.Execute(context.Background(), job, stream); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if called {
		t.Fatal("expected no recorder call when the engine never cleared readiness")
	}
}

// TestLLMInferenceHandler_DefaultRecorderDoesNotPanic covers the production
// default: a handler built via NewLLMInferenceHandler wires
// status.RecordEngineRequest automatically, and must not panic or otherwise
// misbehave when Execute runs without an explicit WithRequestRecorder call.
func TestLLMInferenceHandler_DefaultRecorderDoesNotPanic(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	ts := newChatCompletionsServer(t, frames, nil)
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL
	if h.requestRecorder == nil {
		t.Fatal("expected NewLLMInferenceHandler to default requestRecorder to status.RecordEngineRequest")
	}

	job := &Job{
		ID:   "job-default-recorder",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":    "bonsai-27b",
			"backend":  "bonsai",
			"stream":   true,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		},
	}
	stream := &MockStreamWriter{}
	if _, err := h.Execute(context.Background(), job, stream); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
}
