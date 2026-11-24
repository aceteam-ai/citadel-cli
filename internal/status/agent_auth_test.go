package status

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func allAgentProviders(calls *int) *AgentProviders {
	bump := func() { *calls++ }
	return &AgentProviders{
		WorkerStatus:  func() any { bump(); return map[string]any{} },
		NodeInfo:      func() any { bump(); return map[string]any{} },
		Doctor:        func() any { bump(); return map[string]any{} },
		Config:        func() any { bump(); return map[string]any{} },
		Logs:          func(LogQuery) (string, error) { bump(); return "x", nil },
		VendorAgents:  func(bool) (any, error) { bump(); return map[string]any{}, nil },
		Resubscribe:   func() (any, error) { bump(); return map[string]any{}, nil },
		WorkerRestart: func() (any, error) { bump(); return map[string]any{}, nil },
		SetLogLevel:   func(bool) any { bump(); return map[string]any{} },
	}
}

func serve(mux *http.ServeMux, r *http.Request) int {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w.Code
}

var readOnlyAgentPaths = []string{"/agent/worker-status", "/agent/node-info", "/agent/doctor", "/agent/vendor-agents"}
var sensitiveAgentPaths = []string{"/agent/config", "/agent/logs", "/agent/set-log-level", "/agent/resubscribe", "/agent/worker-restart"}

func agentMethod(path string) string {
	switch path {
	case "/agent/set-log-level", "/agent/resubscribe", "/agent/worker-restart":
		return http.MethodPost
	}
	return http.MethodGet
}

// A CGNAT source the verifier does not vouch for is refused on every /agent/*
// route, and no provider runs.
func TestAgentRefusesUnverifiedMeshSource(t *testing.T) {
	var calls int
	_, mux := newAgentMux(allAgentProviders(&calls))
	for _, path := range append(append([]string{}, readOnlyAgentPaths...), sensitiveAgentPaths...) {
		r := httptest.NewRequest(agentMethod(path), path, nil)
		r.RemoteAddr = "100.64.0.99:4000"
		if code := serve(mux, r); code != http.StatusForbidden {
			t.Errorf("%s: want 403 for unverified mesh source, got %d", path, code)
		}
	}
	if calls != 0 {
		t.Fatalf("providers ran %d time(s) for refused requests", calls)
	}
}

// With no verifier configured, mesh origin never admits anyone (fail closed).
func TestAgentNilVerifierFailsClosed(t *testing.T) {
	var calls int
	s := NewServer(ServerConfig{Port: 8080, Agent: allAgentProviders(&calls)}, NewCollector(CollectorConfig{NodeName: "n"}))
	mux := http.NewServeMux()
	s.registerAgentRoutes(mux)
	if code := serve(mux, vpnReq(http.MethodGet, "/agent/worker-status")); code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", code)
	}
}

// A verifier-approved peer reaches the read-only routes but a sensitive route
// on the plaintext listener is a fixed 403 pointing at the control listener.
func TestAgentVerifiedPeerReadOnlyOnly(t *testing.T) {
	var calls int
	_, mux := newAgentMux(allAgentProviders(&calls))
	for _, path := range readOnlyAgentPaths {
		if code := serve(mux, vpnReq(http.MethodGet, path)); code != http.StatusOK {
			t.Errorf("%s: verified peer want 200, got %d", path, code)
		}
	}
	before := calls
	for _, path := range sensitiveAgentPaths {
		if code := serve(mux, vpnReq(agentMethod(path), path)); code != http.StatusForbidden {
			t.Errorf("%s: want 403 stub on plaintext listener, got %d", path, code)
		}
	}
	if calls != before {
		t.Fatalf("sensitive providers ran on the plaintext listener")
	}
}

// A verified source address does not survive a forwarding header.
func TestAgentForwardedRequestRefused(t *testing.T) {
	var calls int
	_, mux := newAgentMux(allAgentProviders(&calls))
	for _, remote := range []string{"100.64.0.5:1", "127.0.0.1:1"} {
		r := httptest.NewRequest(http.MethodGet, "/agent/worker-status", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		if code := serve(mux, r); code != http.StatusForbidden {
			t.Errorf("%s forwarded: want 403, got %d", remote, code)
		}
	}
}

// Loopback is not an identity: every /agent/* route refuses it, including with
// a loopback Host, a non-loopback Host, an Origin header, or IPv6 loopback.
func TestAgentLoopbackRefused(t *testing.T) {
	var calls int
	_, mux := newAgentMux(allAgentProviders(&calls))
	for _, path := range append(append([]string{}, readOnlyAgentPaths...), sensitiveAgentPaths...) {
		for name, mod := range map[string]func(*http.Request){
			"plain":        func(*http.Request) {},
			"loopbackHost": func(r *http.Request) { r.Host = "127.0.0.1:8080" },
			"rebindHost":   func(r *http.Request) { r.Host = "attacker.example:8080" },
			"origin":       func(r *http.Request) { r.Header.Set("Origin", "http://attacker.example") },
			"ipv6":         func(r *http.Request) { r.RemoteAddr = "[::1]:5" },
		} {
			r := loopbackReq(agentMethod(path), path)
			mod(r)
			if code := serve(mux, r); code != http.StatusForbidden {
				t.Errorf("%s (%s): loopback want 403, got %d", path, name, code)
			}
		}
	}
	if calls != 0 {
		t.Fatalf("providers ran %d time(s) for loopback requests", calls)
	}
}

// A LAN source is refused everywhere.
func TestAgentLANRefused(t *testing.T) {
	var calls int
	_, mux := newAgentMux(allAgentProviders(&calls))
	for _, path := range append(append([]string{}, readOnlyAgentPaths...), sensitiveAgentPaths...) {
		r := httptest.NewRequest(agentMethod(path), path, nil)
		r.RemoteAddr = "192.168.1.50:1234"
		if code := serve(mux, r); code != http.StatusForbidden {
			t.Errorf("%s: LAN want 403, got %d", path, code)
		}
	}
}

// The control mux serves every /agent/* route behind the coordinator gate and
// the gate runs before the handler.
func TestAgentControlRoutesBehindCoordinatorGate(t *testing.T) {
	var calls int
	s := NewServer(ServerConfig{Port: 8080, Agent: allAgentProviders(&calls)}, NewCollector(CollectorConfig{NodeName: "n"}))
	deny := func(http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }
	}
	passthrough := func(next http.HandlerFunc) http.HandlerFunc { return next }

	mux := http.NewServeMux()
	s.registerAgentControlRoutes(mux, deny)
	for _, path := range append(append([]string{}, readOnlyAgentPaths...), sensitiveAgentPaths...) {
		if code := serve(mux, vpnReq(agentMethod(path), path)); code != http.StatusForbidden {
			t.Errorf("%s: denied gate want 403, got %d", path, code)
		}
	}
	if calls != 0 {
		t.Fatalf("handler ran behind a denying gate")
	}

	mux = http.NewServeMux()
	s.registerAgentControlRoutes(mux, passthrough)
	for _, path := range append(append([]string{}, readOnlyAgentPaths...), sensitiveAgentPaths...) {
		if code := serve(mux, vpnReq(agentMethod(path), path)); code != http.StatusOK {
			t.Errorf("%s: control route want 200, got %d", path, code)
		}
	}
}

// bindAddr is loopback by default; the LAN bind is an explicit opt-in.
func TestStatusBindAddr(t *testing.T) {
	def := NewServer(ServerConfig{Port: 8123}, NewCollector(CollectorConfig{NodeName: "n"}))
	if got := def.bindAddr(); got != "127.0.0.1:8123" {
		t.Fatalf("default bind = %q, want loopback", got)
	}
	lan := NewServer(ServerConfig{Port: 8123, BindLAN: true}, NewCollector(CollectorConfig{NodeName: "n"}))
	if got := lan.bindAddr(); got != ":8123" {
		t.Fatalf("LAN opt-in bind = %q, want all interfaces", got)
	}
}
