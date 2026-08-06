package worker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
// whose API answers, for EVERY backend, including the four that had no
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
//
// Connection refused is in the NOT-warming set as of citadel-cli#705: it means
// nothing is listening, which no amount of retrying fixes.
func TestIsEngineNotServing(t *testing.T) {
	warming := []error{
		errors.New(`Post "http://localhost:8213/v1/chat/completions": readfrom tcp 127.0.0.1:57222->127.0.0.1:8213: write tcp: use of closed network connection`),
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
		errors.New("dial tcp 127.0.0.1:8213: connect: connection refused"),
		// The error a REAL dial to an unbound port produces. The literal string
		// above is not enough on its own: a refused dial arrives as *url.Error
		// wrapping *net.OpError, and the net.OpError catch-all in
		// isEngineNotServing would swallow it, so this test would pass while
		// production still classified refused as warming (citadel-cli#705).
		realRefusedError(t),
	}
	for _, err := range notWarming {
		if isEngineNotServing(err) {
			t.Errorf("isEngineNotServing(%v) = true, want false", err)
		}
	}
}

// unboundAddr returns a host:port that is guaranteed to refuse connections: a
// listener is bound to pick a free port, then closed. This is how the
// never-started engine is reproduced without stopping anything real.
func unboundAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// realRefusedError produces a genuine connection-refused error from the HTTP
// client, wrappers and all.
func realRefusedError(t *testing.T) error {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+unboundAddr(t)+"/health", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("dial to an unbound port unexpectedly succeeded")
	}
	return err
}

// startTracker is a scriptable engineStartTracker + modelSwapper, standing in
// for the swap manager's record of "did this node actually start that engine".
type startTracker struct {
	startedAt time.Time
	known     bool
}

func (s *startTracker) EnsureResident(_ context.Context, _, _ string) (SwapOutcome, error) {
	return SwapOutcome{Ready: true}, nil
}

func (s *startTracker) EngineStartedAt(string) (time.Time, bool) {
	return s.startedAt, s.known
}

// TestIsConnectionRefused asserts the refused predicate fires on a real dial
// error and not on the transient signals that mean "a server, mid-start".
func TestIsConnectionRefused(t *testing.T) {
	if !isConnectionRefused(realRefusedError(t)) {
		t.Error("a real refused dial must be recognised as connection refused")
	}
	for _, err := range []error{
		nil,
		errors.New("use of closed network connection"),
		errors.New("EOF"),
		&net.OpError{Op: "read", Err: errors.New("boom")},
	} {
		if isConnectionRefused(err) {
			t.Errorf("isConnectionRefused(%v) = true, want false", err)
		}
	}
}

// TestProbeEngine_NoListenerIsDistinctFromBadResponse is the probe half of
// citadel-cli#705: collapsing both to false is what let the caller treat a
// never-started engine like a loading one.
func TestProbeEngine_NoListenerIsDistinctFromBadResponse(t *testing.T) {
	h := NewLLMInferenceHandler()

	if got := h.probeEngine(context.Background(), "http://"+unboundAddr(t)+"/health"); got != probeNoListener {
		t.Errorf("probe of an unbound port = %v, want probeNoListener", got)
	}

	// Bound but not serving: accept and immediately close.
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
	if got := h.probeEngine(context.Background(), "http://"+ln.Addr().String()+"/health"); got != probeNotServing {
		t.Errorf("probe of a bound-but-not-serving port = %v, want probeNotServing", got)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serveReadinessProbe(w, r) {
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer ts.Close()
	if got := h.probeEngine(context.Background(), ts.URL+"/health"); got != probeServing {
		t.Errorf("probe of a serving engine = %v, want probeServing", got)
	}
}

// TestEnsureEngineReady_NeverStartedEngineFailsFast is the headline case of
// citadel-cli#705. Nothing is listening and no start was ever attempted, so the
// gate must name the engine and error rather than quote an ETA that will never
// arrive. The budget is left at its real value on purpose: the answer must come
// back immediately, not after waiting it out.
func TestEnsureEngineReady_NeverStartedEngineFailsFast(t *testing.T) {
	addr := unboundAddr(t)

	for _, backend := range []string{"vllm", "sglang", "ollama", "llamacpp", "bonsai", "unlimited-ocr"} {
		t.Run(backend, func(t *testing.T) {
			h := NewLLMInferenceHandler()
			h.baseURLs[backend] = "http://" + addr

			start := time.Now()
			err := h.ensureEngineReady(context.Background(), backend)
			if !errors.Is(err, errEngineNotRunning) {
				t.Fatalf("ensureEngineReady(%s) = %v, want errEngineNotRunning", backend, err)
			}
			if errors.Is(err, errEngineWarming) {
				t.Fatalf("a never-started engine must not report warming: %v", err)
			}
			if !strings.Contains(err.Error(), backend) {
				t.Errorf("error must name the engine, got %q", err)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("must fail fast, took %v", elapsed)
			}
		})
	}
}

// TestEnsureEngineReady_StartingEngineStillWarms is the other half of the
// contract: while a start this node issued is inside its load window, an unbound
// port is a cold start, and the caller must still be told to retry. Without this
// the fix would trade an infinite warm for a spurious hard failure on every
// normal cold start.
func TestEnsureEngineReady_StartingEngineStillWarms(t *testing.T) {
	shortenReadinessBudget(t, 50*time.Millisecond)
	addr := unboundAddr(t)

	h := NewLLMInferenceHandler()
	h.baseURLs["unlimited-ocr"] = "http://" + addr
	h.swapper = &startTracker{startedAt: time.Now(), known: true}

	err := h.ensureEngineReady(context.Background(), "unlimited-ocr")
	if !errors.Is(err, errEngineWarming) {
		t.Fatalf("ensureEngineReady = %v, want errEngineWarming", err)
	}
}

// TestEnsureEngineReady_StaleStartFailsFast asserts the start record expires. A
// start issued longer ago than the engine's own load window did not take, so the
// port being unbound is a fault, not a cold start.
func TestEnsureEngineReady_StaleStartFailsFast(t *testing.T) {
	addr := unboundAddr(t)

	h := NewLLMInferenceHandler()
	h.baseURLs["unlimited-ocr"] = "http://" + addr
	stale := time.Now().Add(-2 * engineStartBudget("unlimited-ocr"))
	h.swapper = &startTracker{startedAt: stale, known: true}

	err := h.ensureEngineReady(context.Background(), "unlimited-ocr")
	if !errors.Is(err, errEngineNotRunning) {
		t.Fatalf("ensureEngineReady = %v, want errEngineNotRunning", err)
	}
}

// TestExecute_NeverStartedEngineFailsWithNamedError is the end-to-end form: the
// caller gets an actionable failure naming the engine, not the model_warming /
// retry_after loop reported in citadel-cli#705.
func TestExecute_NeverStartedEngineFailsWithNamedError(t *testing.T) {
	h := NewLLMInferenceHandler()
	h.baseURLs["unlimited-ocr"] = "http://" + unboundAddr(t)

	job := &Job{
		ID:   "job-ocr-dead",
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
	if result.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure (output=%v)", result.Status, result.Output)
	}
	if got, _ := result.Output["status"].(string); got == "model_warming" {
		t.Fatal("a never-started engine must not be reported as model_warming")
	}
	msg, _ := result.Output["error"].(string)
	if !strings.Contains(msg, "unlimited-ocr") {
		t.Errorf("error must name the engine, got %q", msg)
	}
	if !strings.Contains(msg, "not running") {
		t.Errorf("error must say the engine is not running, got %q", msg)
	}
	if len(stream.chunks) != 0 {
		t.Fatalf("a failure must emit no content chunks, got %v", stream.chunks)
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
