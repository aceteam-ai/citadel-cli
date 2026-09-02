package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// errSwapFailedForTest is a sentinel error a fakeModelSwapper can return to
// simulate a hard swap failure (rate-limited, preflight-blocked, ...).
var errSwapFailedForTest = errors.New("swap failed for test")

// portOf extracts the numeric port from an httptest server URL (http://127.0.0.1:NNN).
func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	addr := strings.TrimPrefix(srv.URL, "http://")
	_, portStr, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("cannot parse port from %q", srv.URL)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port %q: %v", portStr, err)
	}
	return p
}

// newChatGateway builds a gateway whose chat routes are registered through the
// SAME registerChatRoutes path Start uses (issue #581), backed by the given
// lister. Returns the gateway ready for gw.mux.ServeHTTP.
func newChatGateway(lister ChatModelLister) *Server {
	gw := NewServer(Config{Port: 0, NodeName: "test-node"})
	gw.SetChatRouter(lister)
	gw.registerChatRoutes()
	gw.mux.HandleFunc("/", gw.handleRoot)
	return gw
}

// newChatGatewayWithFallback is newChatGateway plus the installed-but-stopped
// fallback wiring (citadel-cli#686): SetInstalledModelLister and
// SetModelSwapper, exercised through the SAME registerChatRoutes path as
// production.
func newChatGatewayWithFallback(lister, installedLister ChatModelLister, swapper ModelSwapper) *Server {
	gw := NewServer(Config{Port: 0, NodeName: "test-node"})
	gw.SetChatRouter(lister)
	gw.SetInstalledModelLister(installedLister)
	gw.SetModelSwapper(swapper)
	gw.registerChatRoutes()
	gw.mux.HandleFunc("/", gw.handleRoot)
	return gw
}

// TestChatCompletionsRoutesToServingEngine verifies a chat request for a served
// model is proxied (verbatim body) to that model's engine host port, and the
// upstream status + body flow back through.
func TestChatCompletionsRoutesToServingEngine(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("engine got path %q, want /v1/chat/completions", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		// Echo back the model the gateway forwarded so we can assert verbatim pass-through.
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"backend": "engine", "model": req.Model})
	}))
	defer engine.Close()

	port := portOf(t, engine)
	gw := newChatGateway(func() []ChatUpstream {
		return []ChatUpstream{{Engine: "bonsai", Port: port, Models: []string{"Bonsai-27B-Q1_0.gguf"}}}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"Bonsai-27B-Q1_0.gguf","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "1.2.3.4:5678"
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v (body=%s)", err, w.Body.String())
	}
	if resp["backend"] != "engine" {
		t.Errorf("routed to %q backend, want engine", resp["backend"])
	}
	if resp["model"] != "Bonsai-27B-Q1_0.gguf" {
		t.Errorf("engine saw model %q, want verbatim Bonsai-27B-Q1_0.gguf", resp["model"])
	}
}

// TestChatCompletionsSubstringMatch verifies a short alias resolves to the
// serving engine via the case-insensitive substring fallback (mirroring
// mesh.FindModel).
func TestChatCompletionsSubstringMatch(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"backend": "engine"})
	}))
	defer engine.Close()

	port := portOf(t, engine)
	gw := newChatGateway(func() []ChatUpstream {
		return []ChatUpstream{{Engine: "bonsai", Port: port, Models: []string{"Bonsai-27B-Q1_0.gguf"}}}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"bonsai-27b"}`))
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("substring alias should route: status = %d, body=%s", w.Code, w.Body.String())
	}
}

// TestChatCompletionsStreamingForwardsSSE verifies streaming (stream:true) SSE
// frames are forwarded through the gateway to the client.
func TestChatCompletionsStreamingForwardsSSE(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, chunk := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
			"data: [DONE]\n\n",
		} {
			io.WriteString(w, chunk)
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer engine.Close()

	port := portOf(t, engine)
	gw := newChatGateway(func() []ChatUpstream {
		return []ChatUpstream{{Engine: "vllm", Port: port, Models: []string{"Qwen/Qwen2.5-7B"}}}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"Qwen/Qwen2.5-7B","stream":true}`))
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream (SSE preserved)", ct)
	}
	// Count forwarded SSE data frames.
	var frames int
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			frames++
		}
	}
	if frames != 3 {
		t.Errorf("forwarded %d SSE data frames, want 3; body=%q", frames, w.Body.String())
	}
}

// TestChatCompletionsUnknownModel404 verifies a request for a model no local
// engine serves returns 404 with the OpenAI-shaped model_not_found error.
func TestChatCompletionsUnknownModel404(t *testing.T) {
	gw := newChatGateway(func() []ChatUpstream {
		return []ChatUpstream{{Engine: "vllm", Port: 9999, Models: []string{"Qwen/Qwen2.5-7B"}}}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"does-not-exist"}`))
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	var resp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad error response: %v (body=%s)", err, w.Body.String())
	}
	if resp.Error.Type != "model_not_found" {
		t.Errorf("error.type = %q, want model_not_found", resp.Error.Type)
	}
	if !strings.Contains(resp.Error.Message, "does-not-exist") {
		t.Errorf("error.message = %q, want it to name the model", resp.Error.Message)
	}
}

// TestChatCompletionsRunningMatchSkipsSwapper verifies a running-engine match
// (scenario a) never consults the installed fallback or the swapper at all —
// the swap machinery must be dead weight on the common, already-serving path.
func TestChatCompletionsRunningMatchSkipsSwapper(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"backend": "engine"})
	}))
	defer engine.Close()
	port := portOf(t, engine)

	swapper := &fakeModelSwapper{}
	gw := newChatGatewayWithFallback(
		func() []ChatUpstream {
			return []ChatUpstream{{Engine: "vllm", Port: port, Models: []string{"Qwen/Qwen2.5-7B"}}}
		},
		func() []ChatUpstream { t.Fatal("installedLister must not be consulted on a running match"); return nil },
		swapper,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"Qwen/Qwen2.5-7B"}`))
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(swapper.calls) != 0 {
		t.Fatalf("swapper.calls = %v, want none (running match should never swap)", swapper.calls)
	}
}

// TestChatCompletionsInstalledFallbackWarms verifies scenario (b): a
// running-engine miss that matches an installed-but-stopped engine calls
// EnsureResident, and — when the swap has not finished within the wait
// budget (Ready=false) — returns a 503 with the model_warming contract
// (top-level status/model/eta_seconds/retry_after, plus a Retry-After
// header), mirroring the worker job path's shape.
func TestChatCompletionsInstalledFallbackWarms(t *testing.T) {
	swapper := &fakeModelSwapper{outcome: SwapOutcome{
		Ready:             false,
		ETASeconds:        42,
		RetryAfterSeconds: 7,
		WarmingFor:        "Bonsai-27B-Q1_0.gguf",
	}}
	gw := newChatGatewayWithFallback(
		func() []ChatUpstream { return nil }, // nothing running
		func() []ChatUpstream {
			return []ChatUpstream{{Engine: "bonsai", Port: 8210, Models: []string{"Bonsai-27B-Q1_0.gguf"}}}
		},
		swapper,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"Bonsai-27B-Q1_0.gguf"}`))
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After header = %q, want %q", got, "7")
	}
	var resp struct {
		Status     string `json:"status"`
		Model      string `json:"model"`
		ETASeconds int    `json:"eta_seconds"`
		RetryAfter int    `json:"retry_after"`
		WarmingFor string `json:"warming_for"`
		Error      struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v (body=%s)", err, w.Body.String())
	}
	if resp.Status != "model_warming" {
		t.Errorf("status = %q, want model_warming", resp.Status)
	}
	if resp.Error.Type != "model_warming" {
		t.Errorf("error.type = %q, want model_warming", resp.Error.Type)
	}
	if resp.Model != "Bonsai-27B-Q1_0.gguf" {
		t.Errorf("model = %q, want Bonsai-27B-Q1_0.gguf", resp.Model)
	}
	if resp.ETASeconds != 42 {
		t.Errorf("eta_seconds = %d, want 42", resp.ETASeconds)
	}
	if resp.RetryAfter != 7 {
		t.Errorf("retry_after = %d, want 7", resp.RetryAfter)
	}
	if resp.WarmingFor != "Bonsai-27B-Q1_0.gguf" {
		t.Errorf("warming_for = %q, want Bonsai-27B-Q1_0.gguf", resp.WarmingFor)
	}
	if len(swapper.calls) != 1 || swapper.calls[0] != "bonsai/Bonsai-27B-Q1_0.gguf" {
		t.Fatalf("swapper.calls = %v, want one call for bonsai/Bonsai-27B-Q1_0.gguf", swapper.calls)
	}
}

// TestChatCompletionsInstalledFallbackAlreadyResidentAfterCheck verifies
// scenario (c): when EnsureResident reports Ready=true (the swap finished
// inside its own wait budget), the request routes normally to the
// now-resident engine instead of a 503.
func TestChatCompletionsInstalledFallbackAlreadyResidentAfterCheck(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"backend": "engine"})
	}))
	defer engine.Close()
	port := portOf(t, engine)

	swapper := &fakeModelSwapper{outcome: SwapOutcome{Ready: true}}
	gw := newChatGatewayWithFallback(
		func() []ChatUpstream { return nil }, // nothing running yet
		func() []ChatUpstream {
			return []ChatUpstream{{Engine: "bonsai", Port: port, Models: []string{"Bonsai-27B-Q1_0.gguf"}}}
		},
		swapper,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"Bonsai-27B-Q1_0.gguf"}`))
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v (body=%s)", err, w.Body.String())
	}
	if resp["backend"] != "engine" {
		t.Errorf("routed to %q backend, want engine (the installed engine, now resident)", resp["backend"])
	}
	if len(swapper.calls) != 1 {
		t.Fatalf("swapper.calls = %v, want exactly one EnsureResident call", swapper.calls)
	}
}

// TestChatCompletionsUnknownModelStill404WithFallbackWired verifies scenario
// (d): a genuinely-unknown model still 404s with model_not_found even when
// the installed fallback and swapper are both wired — they must never turn a
// real miss into anything else, and the swapper must not be invoked for a
// model nothing (running or installed) claims to serve.
func TestChatCompletionsUnknownModelStill404WithFallbackWired(t *testing.T) {
	swapper := &fakeModelSwapper{}
	gw := newChatGatewayWithFallback(
		func() []ChatUpstream {
			return []ChatUpstream{{Engine: "vllm", Port: 9999, Models: []string{"Qwen/Qwen2.5-7B"}}}
		},
		func() []ChatUpstream {
			return []ChatUpstream{{Engine: "bonsai", Port: 8210, Models: []string{"Bonsai-27B-Q1_0.gguf"}}}
		},
		swapper,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"does-not-exist"}`))
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad error response: %v (body=%s)", err, w.Body.String())
	}
	if resp.Error.Type != "model_not_found" {
		t.Errorf("error.type = %q, want model_not_found", resp.Error.Type)
	}
	if len(swapper.calls) != 0 {
		t.Fatalf("swapper.calls = %v, want none for a genuinely-unknown model", swapper.calls)
	}
}

// TestChatCompletionsInstalledFallbackSwapErrorIs503 verifies a swap that
// fails outright (rate-limited, preflight-blocked, or any other hard error)
// yields a 503 upstream_error rather than a panic or a misleading 404.
func TestChatCompletionsInstalledFallbackSwapErrorIs503(t *testing.T) {
	swapper := &fakeModelSwapper{err: errSwapFailedForTest}
	gw := newChatGatewayWithFallback(
		func() []ChatUpstream { return nil },
		func() []ChatUpstream {
			return []ChatUpstream{{Engine: "bonsai", Port: 8210, Models: []string{"Bonsai-27B-Q1_0.gguf"}}}
		},
		swapper,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"Bonsai-27B-Q1_0.gguf"}`))
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad error response: %v (body=%s)", err, w.Body.String())
	}
	if resp.Error.Type != "upstream_error" {
		t.Errorf("error.type = %q, want upstream_error", resp.Error.Type)
	}
}

// TestChatCompletionsEmptyModelNeverTriggersInstalledFallback guards against a
// real hazard: resolveChatModel's "empty model routes unambiguously to the
// only candidate" convenience (a reasonable fit for an ALREADY-RUNNING
// single-engine node, where routing changes nothing) must NOT extend to the
// installed fallback, where matching would mean evicting/starting an engine
// in response to a request that simply omitted "model". A request with no
// model and no running engine must 404, never swap.
func TestChatCompletionsEmptyModelNeverTriggersInstalledFallback(t *testing.T) {
	swapper := &fakeModelSwapper{}
	gw := newChatGatewayWithFallback(
		func() []ChatUpstream { return nil }, // nothing running
		func() []ChatUpstream {
			// Exactly one installed-but-stopped candidate -- the shape that
			// would resolve unambiguously if empty-model routing applied here.
			return []ChatUpstream{{Engine: "bonsai", Port: 8210, Models: []string{"Bonsai-27B-Q1_0.gguf"}}}
		},
		swapper,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if len(swapper.calls) != 0 {
		t.Fatalf("swapper.calls = %v, want none for an empty model", swapper.calls)
	}
}

// TestResolveChatModel exercises the pure resolver: exact match, empty-model
// single-engine routing, empty-model ambiguity, and miss.
func TestResolveChatModel(t *testing.T) {
	engines := []ChatUpstream{
		{Engine: "vllm", Port: 8100, Models: []string{"Qwen/Qwen2.5-7B"}},
		{Engine: "bonsai", Port: 8210, Models: []string{"Bonsai-27B-Q1_0.gguf"}},
	}

	if p, e, ok := resolveChatModel("bonsai-27b-q1_0.gguf", engines); !ok || p != 8210 || e != "bonsai" {
		t.Errorf("exact (case-insensitive) = (%d,%q,%v), want (8210,bonsai,true)", p, e, ok)
	}
	if _, _, ok := resolveChatModel("nope", engines); ok {
		t.Error("miss should return ok=false")
	}
	// Empty model with a single engine routes to it.
	single := []ChatUpstream{{Engine: "vllm", Port: 8100, Models: []string{"Qwen/Qwen2.5-7B"}}}
	if p, _, ok := resolveChatModel("", single); !ok || p != 8100 {
		t.Errorf("empty-model single-engine = (%d,%v), want (8100,true)", p, ok)
	}
	// Empty model with two engines is ambiguous -> miss.
	if _, _, ok := resolveChatModel("", engines); ok {
		t.Error("empty-model multi-engine should be ambiguous (ok=false)")
	}
}
