package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

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
