package status

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newAgentMux builds a mux with only the agent routes registered, for testing.
func newAgentMux(p *AgentProviders) (*Server, *http.ServeMux) {
	s := NewServer(ServerConfig{Port: 8080, Version: "test", Agent: p}, NewCollector(CollectorConfig{NodeName: "n"}))
	mux := http.NewServeMux()
	s.registerAgentRoutes(mux)
	return s, mux
}

// vpnReq builds a request that appears to originate from the Headscale VPN
// (100.64.0.0/10), which requireVPNOrAuth trusts without a token.
func vpnReq(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "100.64.0.5:54321"
	return r
}

func TestAgentWorkerStatusOverVPN(t *testing.T) {
	called := false
	_, mux := newAgentMux(&AgentProviders{
		WorkerStatus: func() any { called = true; return map[string]any{"consuming": true} },
	})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, vpnReq(http.MethodGet, "/agent/worker-status"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !called {
		t.Fatalf("provider not invoked")
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if body["consuming"] != true {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestAgentRequiresAuthFromLAN(t *testing.T) {
	_, mux := newAgentMux(&AgentProviders{
		WorkerStatus: func() any { return map[string]any{} },
	})

	// A non-VPN origin without a token must be rejected (no tokenValidator set).
	r := httptest.NewRequest(http.MethodGet, "/agent/worker-status", nil)
	r.RemoteAddr = "192.168.1.50:1234"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 from LAN without token, got %d", w.Code)
	}
}

func TestAgentNilProviderReturns503(t *testing.T) {
	// Providers struct present but WorkerStatus nil -> 503.
	_, mux := newAgentMux(&AgentProviders{})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, vpnReq(http.MethodGet, "/agent/worker-status"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for unwired provider, got %d", w.Code)
	}
}

func TestAgentControlRequiresPost(t *testing.T) {
	_, mux := newAgentMux(&AgentProviders{
		Resubscribe: func() (any, error) { return map[string]any{"ok": true}, nil },
	})
	// GET on a control endpoint must be 405.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, vpnReq(http.MethodGet, "/agent/resubscribe"))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET on control endpoint, got %d", w.Code)
	}
	// POST works.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, vpnReq(http.MethodPost, "/agent/resubscribe"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for POST resubscribe, got %d", w.Code)
	}
}

func TestAgentLogsQueryParams(t *testing.T) {
	var got LogQuery
	_, mux := newAgentMux(&AgentProviders{
		Logs: func(q LogQuery) (string, error) { got = q; return "line1\nline2", nil },
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, vpnReq(http.MethodGet, "/agent/logs?lines=50&level=error&grep=consume&since=5m"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got.Lines != 50 || got.Level != "error" || got.Grep != "consume" || got.Since != "5m" {
		t.Fatalf("query params not parsed: %+v", got)
	}
}

func TestRegisterAgentRoutesNoopWhenNil(t *testing.T) {
	// No panic and no routes when providers are nil.
	s := NewServer(ServerConfig{Port: 8080}, NewCollector(CollectorConfig{NodeName: "n"}))
	mux := http.NewServeMux()
	s.registerAgentRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, vpnReq(http.MethodGet, "/agent/worker-status"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (route not registered) when providers nil, got %d", w.Code)
	}
}
