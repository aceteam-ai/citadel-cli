package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLLMInferenceHandler_CanHandle asserts the handler only claims the
// "llm_inference" job type (issue #590).
func TestLLMInferenceHandler_CanHandle(t *testing.T) {
	h := NewLLMInferenceHandler()
	if !h.CanHandle(JobTypeLLMInference) {
		t.Errorf("CanHandle(%q) = false, want true", JobTypeLLMInference)
	}
	for _, other := range []string{"SHELL_COMMAND", "embedding", "VLLM_INFERENCE", ""} {
		if h.CanHandle(other) {
			t.Errorf("CanHandle(%q) = true, want false", other)
		}
	}
}

// newChatCompletionsServer returns an httptest server that emits the given SSE
// frames verbatim (each already a full "data: ..." line) at /v1/chat/completions.
// It records the last request path so routing can be asserted.
func newChatCompletionsServer(t *testing.T, frames []string, lastPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lastPath != nil {
			*lastPath = r.URL.Path
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = w.Write([]byte(f + "\n"))
		}
	}))
}

// TestLLMInferenceHandler_ChatStreaming drives a streaming chat request through
// the bonsai backend (routes to /v1/chat/completions with no readiness poll) and
// asserts the SSE deltas are forwarded as chunks and accumulated into the final
// JobResult.Output. NOTE: the handler intentionally does NOT call WriteEnd — the
// Runner does that with result.Output (runner.go), so the streaming test asserts
// the WriteChunk captures plus the returned Output.content, not a WriteEnd call.
func TestLLMInferenceHandler_ChatStreaming(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		`data: {"choices":[{"delta":{"content":", world"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	var gotPath string
	ts := newChatCompletionsServer(t, frames, &gotPath)
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL

	job := &Job{
		ID:   "job-1",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":   "bonsai-27b",
			"backend": "bonsai",
			"stream":  true,
			"messages": []map[string]any{
				{"role": "user", "content": "hi"},
			},
		},
	}
	stream := &MockStreamWriter{}
	result, err := h.Execute(context.Background(), job, stream)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("Execute result = %+v, want success", result)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("request path = %q, want /v1/chat/completions", gotPath)
	}
	wantChunks := []string{"Hello", ", world"}
	if strings.Join(stream.chunks, "|") != strings.Join(wantChunks, "|") {
		t.Errorf("chunks = %v, want %v", stream.chunks, wantChunks)
	}
	if got, _ := result.Output["content"].(string); got != "Hello, world" {
		t.Errorf("Output content = %q, want %q", got, "Hello, world")
	}
	if got, _ := result.Output["finish_reason"].(string); got != "stop" {
		t.Errorf("Output finish_reason = %q, want %q", got, "stop")
	}
}

// TestLLMInferenceHandler_ChatStreamingReasoningFallback covers the thinking-model
// path: when a stream carries only delta.reasoning_content and no answer (token
// budget spent mid-reasoning, e.g. Bonsai), the reasoning is surfaced as the final
// content so the reply is never blank — mirroring the buffered fallback.
func TestLLMInferenceHandler_ChatStreamingReasoningFallback(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"reasoning_content":"thinking hard..."}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
		`data: [DONE]`,
	}
	ts := newChatCompletionsServer(t, frames, nil)
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL

	job := &Job{
		ID:   "job-reason",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":    "bonsai-27b",
			"backend":  "bonsai",
			"stream":   true,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		},
	}
	stream := &MockStreamWriter{}
	result, err := h.Execute(context.Background(), job, stream)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("result = %+v, want success", result)
	}
	if got, _ := result.Output["content"].(string); got != "thinking hard..." {
		t.Errorf("Output content = %q, want reasoning fallback", got)
	}
	// The reasoning is surfaced as a single chunk once no answer was produced.
	if len(stream.chunks) != 1 || stream.chunks[0] != "thinking hard..." {
		t.Errorf("chunks = %v, want [thinking hard...]", stream.chunks)
	}
}

// TestLLMInferenceHandler_BackendRouting asserts that both the bonsai and
// llamacpp backends resolve to the /v1/chat/completions path when the job
// carries messages, and that an unknown backend fails.
func TestLLMInferenceHandler_BackendRouting(t *testing.T) {
	// A non-streamed OpenAI chat-completions body (single buffered response).
	body := `{"choices":[{"message":{"content":"routed-ok"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`

	for _, backend := range []string{"bonsai", "llamacpp"} {
		t.Run(backend, func(t *testing.T) {
			var gotPath string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				if r.URL.Path != "/v1/chat/completions" {
					http.Error(w, "unexpected path", http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(body))
			}))
			defer ts.Close()

			h := NewLLMInferenceHandler()
			h.baseURLs[backend] = ts.URL

			job := &Job{
				ID:   "job-" + backend,
				Type: JobTypeLLMInference,
				Payload: map[string]any{
					"model":   "m",
					"backend": backend,
					"messages": []map[string]any{
						{"role": "user", "content": "hi"},
					},
				},
			}
			stream := &MockStreamWriter{}
			result, err := h.Execute(context.Background(), job, stream)
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if result == nil || result.Status != JobStatusSuccess {
				t.Fatalf("result = %+v, want success", result)
			}
			if gotPath != "/v1/chat/completions" {
				t.Errorf("%s routed to %q, want /v1/chat/completions", backend, gotPath)
			}
			if got, _ := result.Output["content"].(string); got != "routed-ok" {
				t.Errorf("content = %q, want routed-ok", got)
			}
			// The buffered path emits a single parity chunk before the end.
			if len(stream.chunks) != 1 || stream.chunks[0] != "routed-ok" {
				t.Errorf("chunks = %v, want [routed-ok]", stream.chunks)
			}
		})
	}

	t.Run("unknown backend fails", func(t *testing.T) {
		h := NewLLMInferenceHandler()
		job := &Job{
			ID:   "job-unknown",
			Type: JobTypeLLMInference,
			Payload: map[string]any{
				"model":   "m",
				"prompt":  "hi",
				"backend": "does-not-exist",
			},
		}
		result, err := h.Execute(context.Background(), job, &MockStreamWriter{})
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if result == nil || result.Status != JobStatusFailure {
			t.Fatalf("result = %+v, want failure for unknown backend", result)
		}
		if !strings.Contains(result.Error.Error(), "unsupported backend") {
			t.Errorf("error = %v, want 'unsupported backend'", result.Error)
		}
	})
}
