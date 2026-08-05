package worker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// serveReadinessProbe answers the readiness endpoints a real serving engine
// exposes, so an httptest engine in these tests behaves like one that is up AND
// serving. Returns true when it handled the request (citadel-cli#680).
func serveReadinessProbe(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/health":
		w.WriteHeader(http.StatusOK)
		return true
	case "/v1/models":
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m","object":"model"}]}`))
		return true
	case "/api/tags":
		_, _ = w.Write([]byte(`{"models":[{"name":"m"}]}`))
		return true
	}
	return false
}

// shortenReadinessBudget makes the "not serving" path fast enough to unit test
// without waiting out the real budget.
func shortenReadinessBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := engineReadyBudgetOverride
	engineReadyBudgetOverride = &d
	t.Cleanup(func() { engineReadyBudgetOverride = prev })
}

// TestEnsureEngineReady_ServingEngineIsReady asserts the probe accepts an engine
// whose API answers, for EVERY backend -- including the four that had no
// pre-flight probe at all before citadel-cli#680.
func TestEnsureEngineReady_ServingEngineIsReady(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReadinessProbe(w, r) {
			return
		}
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
	}))
	defer ts.Close()

	for _, backend := range []string{"vllm", "sglang", "ollama", "llamacpp", "bonsai", "unlimited-ocr"} {
		t.Run(backend, func(t *testing.T) {
			h := NewLLMInferenceHandler()
			h.baseURLs[backend] = ts.URL
			if err := h.ensureEngineReady(context.Background(), backend); err != nil {
				t.Fatalf("ensureEngineReady(%s) = %v, want nil", backend, err)
			}
		})
	}
}

// TestEnsureEngineReady_EveryBackendIsProbed is the regression guard for the
// actual #680 defect: only vllm and sglang were probed, so a loading
// unlimited-ocr / llamacpp / bonsai / ollama was proxied into and the caller got
// a socket error. Every backend must now report warming, not readiness.
func TestEnsureEngineReady_EveryBackendIsProbed(t *testing.T) {
	shortenReadinessBudget(t, 50*time.Millisecond)

	// A listener that accepts and immediately closes: a port that is bound but
	// not serving, which is exactly the 78s window measured on node 1297.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	for _, backend := range []string{"vllm", "sglang", "ollama", "llamacpp", "bonsai", "unlimited-ocr"} {
		t.Run(backend, func(t *testing.T) {
			h := NewLLMInferenceHandler()
			h.baseURLs[backend] = "http://" + ln.Addr().String()
			err := h.ensureEngineReady(context.Background(), backend)
			if !errors.Is(err, errEngineWarming) {
				t.Fatalf("ensureEngineReady(%s) = %v, want errEngineWarming", backend, err)
			}
		})
	}
}

// TestEnsureEngineReady_EmptyModelListIsStillReady is the deferred-load guard.
// llama.cpp and bonsai can be up, answer /v1/models with an EMPTY list, and
// still serve (they load on first request). Gating readiness on a non-empty
// model list would make such an engine permanently unservable.
func TestEnsureEngineReady_EmptyModelListIsStillReady(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["llamacpp"] = ts.URL
	if err := h.ensureEngineReady(context.Background(), "llamacpp"); err != nil {
		t.Fatalf("deferred-load llama.cpp must be servable, got %v", err)
	}
}

// TestEnsureEngineReady_UnknownBackendFailsOpen asserts a backend with no known
// readiness endpoint is never made unservable by this gate.
func TestEnsureEngineReady_UnknownBackendFailsOpen(t *testing.T) {
	h := NewLLMInferenceHandler()
	if err := h.ensureEngineReady(context.Background(), "some-future-engine"); err != nil {
		t.Fatalf("unknown backend must fail open, got %v", err)
	}
}

// TestExecute_LoadingEngineReturnsWarmingNotSocketError is the end-to-end form
// of #7220's third finding: a bound-but-not-serving port must reach the caller
// as a typed warming result, never as `use of closed network connection`.
func TestExecute_LoadingEngineReturnsWarmingNotSocketError(t *testing.T) {
	shortenReadinessBudget(t, 50*time.Millisecond)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	h := NewLLMInferenceHandler()
	h.baseURLs["unlimited-ocr"] = "http://" + ln.Addr().String()

	job := &Job{
		ID:   "job-ocr",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":    "baidu/Unlimited-OCR",
			"backend":  "unlimited-ocr",
			"messages": []map[string]any{{"role": "user", "content": "read this"}},
		},
	}
	stream := &MockStreamWriter{}
	result, execErr := h.Execute(context.Background(), job, stream)
	if execErr != nil {
		t.Fatalf("Execute error: %v", execErr)
	}
	if result.Status != JobStatusSuccess {
		t.Fatalf("warming must be a SUCCESS result, got %v (err=%v)", result.Status, result.Error)
	}
	if got, _ := result.Output["status"].(string); got != "model_warming" {
		t.Fatalf("status = %q, want model_warming", got)
	}
	if got, _ := result.Output["model"].(string); got != "baidu/Unlimited-OCR" {
		t.Fatalf("model = %q, want baidu/Unlimited-OCR", got)
	}
	if eta, _ := result.Output["eta_seconds"].(int); eta <= 0 {
		t.Fatalf("eta_seconds = %v, want a positive estimate", eta)
	}
	if ra, _ := result.Output["retry_after"].(int); ra <= 0 {
		t.Fatalf("retry_after = %v, want a positive hint", ra)
	}
	if len(stream.chunks) != 0 {
		t.Fatalf("warming must emit no content chunks, got %v", stream.chunks)
	}
	if result.Error != nil {
		t.Fatalf("warming must carry no error, got %v", result.Error)
	}
}

// TestIsEngineNotServing classifies transport errors. The literal string from
// #7220 must be recognised, and job cancellation must NOT be, or a cancelled
// job would be reported to the caller as a model that is merely loading.
func TestIsEngineNotServing(t *testing.T) {
	warming := []error{
		errors.New(`Post "http://localhost:8213/v1/chat/completions": readfrom tcp 127.0.0.1:57222->127.0.0.1:8213: write tcp: use of closed network connection`),
		errors.New("dial tcp 127.0.0.1:8213: connect: connection refused"),
		errors.New("read tcp: connection reset by peer"),
		&net.OpError{Op: "dial", Err: errors.New("boom")},
	}
	for _, err := range warming {
		if !isEngineNotServing(err) {
			t.Errorf("isEngineNotServing(%v) = false, want true", err)
		}
	}

	notWarming := []error{
		nil,
		context.Canceled,
		context.DeadlineExceeded,
		errors.New("failed to parse llama.cpp response: invalid character"),
	}
	for _, err := range notWarming {
		if isEngineNotServing(err) {
			t.Errorf("isEngineNotServing(%v) = true, want false", err)
		}
	}
}

// TestRetryAfterFor asserts the retry hint is paced to the quoted wait and
// bounded, so a caller queued behind another model is neither polling every 10s
// nor told to disappear indefinitely.
func TestRetryAfterFor(t *testing.T) {
	if got := retryAfterFor(3); got != warmingRetryAfter {
		t.Errorf("retryAfterFor(3) = %d, want %d", got, warmingRetryAfter)
	}
	if got := retryAfterFor(45); got != 45 {
		t.Errorf("retryAfterFor(45) = %d, want 45", got)
	}
	if got := retryAfterFor(9999); got != warmingRetryAfterMax {
		t.Errorf("retryAfterFor(9999) = %d, want %d", got, warmingRetryAfterMax)
	}
}
