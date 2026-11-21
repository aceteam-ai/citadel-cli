package status

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

const selfMeshIP = "100.64.0.42"

// newExposeServer registers the agent routes with Expose/Unexpose stubs that
// record whether they ran, and a self-mesh-IP source (nil = unknown).
func newExposeServer(self func() []netip.Addr) (*http.ServeMux, *int) {
	calls := 0
	p := &AgentProviders{
		Expose:   func(ExposeSpec) (any, error) { calls++; return map[string]string{"url": "ok"}, nil },
		Unexpose: func(string) (any, error) { calls++; return map[string]string{"ok": "true"}, nil },
	}
	s := NewServer(ServerConfig{Port: 8080, Version: "test", Agent: p, SelfMeshIPs: self},
		NewCollector(CollectorConfig{NodeName: "n"}))
	mux := http.NewServeMux()
	s.registerAgentRoutes(mux)
	return mux, &calls
}

func selfIPs() []netip.Addr { return []netip.Addr{netip.MustParseAddr(selfMeshIP)} }

func exposeReq(path, remote string, hdr map[string]string) *http.Request {
	body := `{"name":"svc","port":8212,"visibility":"org"}`
	if path == "/agent/unexpose" {
		body = `{"name":"svc"}`
	}
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	r.RemoteAddr = remote
	local := selfMeshIP + ":8080"
	if v, ok := hdr["__local"]; ok {
		local = v
	}
	if local != "" {
		la, _ := net.ResolveTCPAddr("tcp", local)
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(la)))
	}
	for k, v := range hdr {
		if k == "__local" {
			continue
		}
		r.Header.Set(k, v)
	}
	return r
}

// The node's own CLI (`citadel service expose`) dials the node's own mesh IP;
// that source is admitted.
func TestExposeAdmitsSelfOrigin(t *testing.T) {
	for _, path := range []string{"/agent/expose", "/agent/unexpose"} {
		mux, calls := newExposeServer(selfIPs)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, exposeReq(path, selfMeshIP+":51000", nil))
		if w.Code != http.StatusOK || *calls != 1 {
			t.Fatalf("%s from self: want 200 and 1 call, got %d and %d: %s", path, w.Code, *calls, w.Body.String())
		}
	}
}

// Any other source is refused before the provider runs: another mesh peer
// (which requireVPNOrAuth used to trust), loopback (the gateway and the
// platform-exposure relay both dial it), a LAN address, a forwarded request
// from the self IP, and any request when the self IP is unknown.
func TestExposeRefusesEverythingButSelfOrigin(t *testing.T) {
	cases := []struct {
		name   string
		self   func() []netip.Addr
		remote string
		hdr    map[string]string
	}{
		{"other mesh peer", selfIPs, "100.64.0.5:51000", nil},
		{"loopback v4", selfIPs, "127.0.0.1:51000", nil},
		{"loopback v6", selfIPs, "[::1]:51000", nil},
		{"LAN", selfIPs, "192.168.1.50:51000", nil},
		{"self but X-Forwarded-For", selfIPs, selfMeshIP + ":51000", map[string]string{"X-Forwarded-For": "100.64.0.5:1"}},
		{"self but Forwarded", selfIPs, selfMeshIP + ":51000", map[string]string{"Forwarded": "for=100.64.0.5"}},
		{"self but X-Real-IP", selfIPs, selfMeshIP + ":51000", map[string]string{"X-Real-IP": "100.64.0.5"}},
		{"self IP unknown (nil)", nil, selfMeshIP + ":51000", nil},
		{"self IP unknown (empty)", func() []netip.Addr { return nil }, selfMeshIP + ":51000", nil},
		{"self IP is loopback", func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("127.0.0.1")} }, "127.0.0.1:51000", nil},
		{"unparseable remote", selfIPs, "not-an-ip", nil},
		{"self source but arrived on the LAN address", selfIPs, selfMeshIP + ":51000", map[string]string{"__local": "192.168.1.10:8080"}},
		{"self source but arrived on loopback", selfIPs, selfMeshIP + ":51000", map[string]string{"__local": "127.0.0.1:8080"}},
		{"self source but no local address", selfIPs, selfMeshIP + ":51000", map[string]string{"__local": ""}},
		{"self but empty X-Forwarded-For", selfIPs, selfMeshIP + ":51000", map[string]string{"X-Forwarded-For": ""}},
	}
	for _, tc := range cases {
		for _, path := range []string{"/agent/expose", "/agent/unexpose"} {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				mux, calls := newExposeServer(tc.self)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, exposeReq(path, tc.remote, tc.hdr))
				if w.Code != http.StatusForbidden {
					t.Fatalf("want 403, got %d: %s", w.Code, w.Body.String())
				}
				if *calls != 0 {
					t.Fatalf("provider ran %d time(s) for a refused request", *calls)
				}
			})
		}
	}
}

// Nonexposure agent endpoints refuse plaintext without invoking a provider.
func TestNonExposeAgentRoutesRefusePlaintext(t *testing.T) {
	called := false
	p := &AgentProviders{WorkerStatus: func() any { called = true; return map[string]any{} }}
	s := NewServer(ServerConfig{Port: 8080, Version: "test", Agent: p, SelfMeshIPs: selfIPs},
		NewCollector(CollectorConfig{NodeName: "n"}))
	mux := http.NewServeMux()
	s.registerAgentRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, vpnReq(http.MethodGet, "/agent/worker-status"))
	if w.Code != http.StatusForbidden || called {
		t.Fatalf("worker-status from a mesh peer: want 403 and no call, got %d and called=%v", w.Code, called)
	}
}

// An IPv4-mapped form of the node's own address on both ends is admitted.
func TestExposeAdmitsMappedSelfOrigin(t *testing.T) {
	mux, calls := newExposeServer(selfIPs)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, exposeReq("/agent/expose", "[::ffff:"+selfMeshIP+"]:51000", map[string]string{"__local": "[::ffff:" + selfMeshIP + "]:8080"}))
	if w.Code != http.StatusOK || *calls != 1 {
		t.Fatalf("mapped self origin: want 200 and 1 call, got %d and %d", w.Code, *calls)
	}
}
