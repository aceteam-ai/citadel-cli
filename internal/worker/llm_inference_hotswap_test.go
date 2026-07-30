package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeSwapper is a scriptable modelSwapper for the handler's hotswap path.
type fakeSwapper struct {
	outcome SwapOutcome
	err     error
	calls   int
	backend string
}

func (f *fakeSwapper) EnsureResident(_ context.Context, backend, _ string) (SwapOutcome, error) {
	f.calls++
	f.backend = backend
	return f.outcome, f.err
}

func hotswapJob() *Job {
	return &Job{
		ID:   "job-hs",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":    "bonsai-27b",
			"backend":  "bonsai",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		},
	}
}

// TestHotswap_FlagOff_NoSwapperIsPassthrough asserts that with no swapper
// injected (CITADEL_MODEL_HOTSWAP off), Execute routes straight to the engine —
// byte-for-byte the pre-#632 behavior, never touching a swap path.
func TestHotswap_FlagOff_NoSwapperIsPassthrough(t *testing.T) {
	body := `{"choices":[{"message":{"content":"served"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL
	if h.swapper != nil {
		t.Fatalf("expected nil swapper by default")
	}

	result, err := h.Execute(context.Background(), hotswapJob(), &MockStreamWriter{})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.Status != JobStatusSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	if got, _ := result.Output["content"].(string); got != "served" {
		t.Fatalf("content = %q, want served", got)
	}
	if s, _ := result.Output["status"].(string); s == "model_warming" {
		t.Fatalf("flag-off path must never emit a warming result")
	}
}

// TestHotswap_NotReady_ReturnsWarmingShape asserts the exact model_warming JSON
// shape and that NO content chunk is emitted (the platform relays warming, it is
// not assistant content).
func TestHotswap_NotReady_ReturnsWarmingShape(t *testing.T) {
	h := NewLLMInferenceHandler().WithSwapper(&fakeSwapper{
		outcome: SwapOutcome{Ready: false, ETASeconds: 42},
	})

	stream := &MockStreamWriter{}
	result, err := h.Execute(context.Background(), hotswapJob(), stream)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.Status != JobStatusSuccess {
		t.Fatalf("warming must be a SUCCESS result, got %v", result.Status)
	}
	if len(stream.chunks) != 0 {
		t.Fatalf("warming must emit no content chunks, got %v", stream.chunks)
	}
	if got, _ := result.Output["status"].(string); got != "model_warming" {
		t.Fatalf("status = %q, want model_warming", got)
	}
	if got, _ := result.Output["model"].(string); got != "bonsai-27b" {
		t.Fatalf("model = %q, want bonsai-27b", got)
	}
	if got, _ := result.Output["eta_seconds"].(int); got != 42 {
		t.Fatalf("eta_seconds = %v, want 42", got)
	}
	if got, _ := result.Output["retry_after"].(int); got != warmingRetryAfter {
		t.Fatalf("retry_after = %v, want %d", got, warmingRetryAfter)
	}
}

// TestHotswap_Ready_RoutesToEngine asserts that when the swap reports Ready the
// handler proceeds to the normal engine path and serves the reply.
func TestHotswap_Ready_RoutesToEngine(t *testing.T) {
	body := `{"choices":[{"message":{"content":"served-after-swap"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	sw := &fakeSwapper{outcome: SwapOutcome{Ready: true}}
	h := NewLLMInferenceHandler().WithSwapper(sw)
	h.baseURLs["bonsai"] = ts.URL

	result, err := h.Execute(context.Background(), hotswapJob(), &MockStreamWriter{})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if sw.calls != 1 || sw.backend != "bonsai" {
		t.Fatalf("expected EnsureResident called once for bonsai, got calls=%d backend=%q", sw.calls, sw.backend)
	}
	if got, _ := result.Output["content"].(string); got != "served-after-swap" {
		t.Fatalf("content = %q, want served-after-swap", got)
	}
}

// TestHotswap_SwapError_FailsJob asserts a hard swap error fails the job.
func TestHotswap_SwapError_FailsJob(t *testing.T) {
	h := NewLLMInferenceHandler().WithSwapper(&fakeSwapper{
		err: errors.New("cannot swap in bonsai: pinned services hold VRAM"),
	})

	result, err := h.Execute(context.Background(), hotswapJob(), &MockStreamWriter{})
	if err != nil {
		t.Fatalf("Execute returned transport error: %v", err)
	}
	if result.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure on hard swap error", result.Status)
	}
}
