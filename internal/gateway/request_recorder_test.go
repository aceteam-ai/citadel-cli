package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleChatCompletions_RecordsResolvedEngine verifies the gateway's chat
// router stamps the node-routed request recorder with the RESOLVED engine
// name (not the raw "model" from the request body) before proxying, so a
// citadel #691 heartbeat reader can attribute the request to the right
// engine even when the model id and the engine name differ.
func TestHandleChatCompletions_RecordsResolvedEngine(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer engine.Close()

	port := portOf(t, engine)
	gw := NewServer(Config{Port: 0, NodeName: "test-node"})
	gw.SetChatRouter(func() []ChatUpstream {
		return []ChatUpstream{{Engine: "bonsai", Port: port, Models: []string{"Bonsai-27B-Q1_0.gguf"}}}
	})

	var recorded []string
	gw.SetRequestRecorder(func(eng string) { recorded = append(recorded, eng) })
	gw.registerChatRoutes()
	gw.mux.HandleFunc("/", gw.handleRoot)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"Bonsai-27B-Q1_0.gguf","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(recorded) != 1 || recorded[0] != "bonsai" {
		t.Fatalf("expected exactly one record for %q, got %v", "bonsai", recorded)
	}
}

// TestHandleChatCompletions_UnknownModel_DoesNotRecord verifies a 404
// (no engine serves the requested model) never fires the recorder: recording
// a request for an engine it was never routed to would fabricate activity.
func TestHandleChatCompletions_UnknownModel_DoesNotRecord(t *testing.T) {
	gw := NewServer(Config{Port: 0, NodeName: "test-node"})
	gw.SetChatRouter(func() []ChatUpstream {
		return []ChatUpstream{{Engine: "bonsai", Port: 1, Models: []string{"Bonsai-27B-Q1_0.gguf"}}}
	})

	called := false
	gw.SetRequestRecorder(func(string) { called = true })
	gw.registerChatRoutes()
	gw.mux.HandleFunc("/", gw.handleRoot)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"no-such-model","messages":[]}`))
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if called {
		t.Fatal("expected no recorder call for a model that resolved to no engine")
	}
}

// TestHandleChatCompletions_NilRecorder_DoesNotPanic covers the default
// (unset) case: production callers that never call SetRequestRecorder must
// keep working exactly as before citadel #691.
func TestHandleChatCompletions_NilRecorder_DoesNotPanic(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer engine.Close()

	port := portOf(t, engine)
	gw := newChatGateway(func() []ChatUpstream {
		return []ChatUpstream{{Engine: "vllm", Port: port, Models: []string{"m"}}}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}
