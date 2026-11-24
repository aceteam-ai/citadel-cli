package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// v1Gateway builds a gateway with the chat routes registered (so an allowed
// request reaches a real handler) and the given resolver, driven through the
// full BuildHandler chain.
func v1Gateway(resolver MeshIdentityResolver) *Server {
	gw := newChatGateway(func() []ChatUpstream { return nil })
	if resolver != nil {
		gw.SetMeshResolver(resolver)
	}
	return gw
}

func v1Req(path, remote string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = remote
	return r
}

func TestV1Auth_NonMeshNonLoopbackRefused(t *testing.T) {
	// A LAN caller: the resolver cannot vouch for it. Includes a nil resolver.
	for name, res := range map[string]MeshIdentityResolver{
		"nil resolver":   nil,
		"resolver error": &borrowAuthResolver{err: errors.New("no such peer")},
	} {
		t.Run(name, func(t *testing.T) {
			gw := v1Gateway(res)
			for _, p := range []string{"/v1/models", "/v1/chat/completions", "/v1/embeddings", "/v1/../v1/models"} {
				rec := httptest.NewRecorder()
				gw.BuildHandler().ServeHTTP(rec, v1Req(p, "192.168.1.50:4444"))
				if rec.Code != http.StatusForbidden {
					t.Errorf("%s: status %d, want 403", p, rec.Code)
				}
			}
		})
	}
}

func TestV1Auth_UnverifiedMeshCallerRefused(t *testing.T) {
	cases := map[string]*MeshPeerIdentity{
		"no stable id":          {LoginName: "a@x", OwnerID: "u:1", SameOwner: true},
		"no owner id":           {LoginName: "a@x", StableID: "n:1", SameOwner: true},
		"foreign owner":         {LoginName: "b@y", StableID: "n:2", OwnerID: "u:2"},
		"same-owner flag unset": {LoginName: "a@x", StableID: "n:1", OwnerID: "u:1"},
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			gw := v1Gateway(&borrowAuthResolver{id: id})
			rec := httptest.NewRecorder()
			gw.BuildHandler().ServeHTTP(rec, v1Req("/v1/models", "100.64.0.9:1234"))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403", rec.Code)
			}
		})
	}
	// Nil identity without error.
	gw := v1Gateway(&borrowAuthResolver{})
	rec := httptest.NewRecorder()
	gw.BuildHandler().ServeHTTP(rec, v1Req("/v1/models", "100.64.0.9:1234"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("nil identity: status %d, want 403", rec.Code)
	}
}

func TestV1Auth_SameOwnerPeerAllowed(t *testing.T) {
	gw := v1Gateway(&borrowAuthResolver{id: sameOwnerID()})
	rec := httptest.NewRecorder()
	gw.BuildHandler().ServeHTTP(rec, v1Req("/v1/models", "100.64.0.9:1234"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestV1Auth_LoopbackAllowedWithoutResolver(t *testing.T) {
	gw := v1Gateway(nil)
	for _, remote := range []string{"127.0.0.1:5555", "[::1]:5555", "[::ffff:127.0.0.1]:5555"} {
		rec := httptest.NewRecorder()
		gw.BuildHandler().ServeHTTP(rec, v1Req("/v1/models", remote))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", remote, rec.Code)
		}
	}
}

func TestV1Auth_ForwardingHeadersIgnored(t *testing.T) {
	gw := v1Gateway(nil)
	r := v1Req("/v1/models", "192.168.1.50:4444")
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	r.Header.Set("X-Real-IP", "127.0.0.1")
	rec := httptest.NewRecorder()
	gw.BuildHandler().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403 (headers must not grant loopback)", rec.Code)
	}
}

func TestV1Auth_PlatformAllowlist(t *testing.T) {
	platform := &MeshPeerIdentity{LoginName: "platform@infra", StableID: "n:9", OwnerID: "u:9"}
	gw := v1Gateway(&borrowAuthResolver{id: platform})

	// No allowlist: refused.
	rec := httptest.NewRecorder()
	gw.BuildHandler().ServeHTTP(rec, v1Req("/v1/models", "100.64.0.9:1"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no allowlist: status %d, want 403", rec.Code)
	}
	// Allowlisted login passes the gate.
	gw.SetV1AllowedLogins(func() (map[string]bool, error) { return map[string]bool{"platform@infra": true}, nil })
	rec = httptest.NewRecorder()
	gw.BuildHandler().ServeHTTP(rec, v1Req("/v1/models", "100.64.0.9:1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("allowlisted: status %d, want 200", rec.Code)
	}
	// Loader error degrades to same-owner only: refused.
	gw.SetV1AllowedLogins(func() (map[string]bool, error) { return nil, errors.New("malformed") })
	rec = httptest.NewRecorder()
	gw.BuildHandler().ServeHTTP(rec, v1Req("/v1/models", "100.64.0.9:1"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("loader error: status %d, want 403", rec.Code)
	}
}

func TestV1Auth_NonV1PathsUnaffected(t *testing.T) {
	gw := v1Gateway(nil)
	rec := httptest.NewRecorder()
	gw.BuildHandler().ServeHTTP(rec, v1Req("/", "192.168.1.50:4444"))
	if rec.Code != http.StatusOK {
		t.Fatalf("root: status %d, want 200 (gate is /v1 only)", rec.Code)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func startForTest(t *testing.T, cfg Config) *Server {
	t.Helper()
	gw := NewServer(cfg)
	gw.SetChatRouter(func() []ChatUpstream { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = gw.Start(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		gw.mu.RLock()
		up := gw.httpServer != nil
		gw.mu.RUnlock()
		if up {
			time.Sleep(100 * time.Millisecond)
			return gw
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("gateway did not start")
	return nil
}

func TestStart_DefaultBindsLoopbackOnly(t *testing.T) {
	port := freePort(t)
	gw := startForTest(t, Config{Port: port})
	if want := fmt.Sprintf("127.0.0.1:%d", port); gw.httpServer.Addr != want {
		t.Fatalf("default listen addr %q, want %q", gw.httpServer.Addr, want)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
	if err != nil {
		t.Fatalf("loopback request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback /v1/models = %d, want 200", resp.StatusCode)
	}
}

func TestStart_LANBindOptInStillWorks(t *testing.T) {
	port := freePort(t)
	gw := startForTest(t, Config{Port: port, ListenAddress: fmt.Sprintf("0.0.0.0:%d", port)})
	if !strings.HasPrefix(gw.httpServer.Addr, "0.0.0.0:") {
		t.Fatalf("opt-in addr %q, want 0.0.0.0", gw.httpServer.Addr)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
	if err != nil {
		t.Fatalf("request on opted-in bind: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
}

func TestListenAddrIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8443": true, "[::1]:8443": true, "localhost:8443": true,
		"0.0.0.0:8443": false, ":8443": false, "192.168.1.5:8443": false, "[::]:8443": false,
	} {
		if got := listenAddrIsLoopback(addr); got != want {
			t.Errorf("%s: got %v want %v", addr, got, want)
		}
	}
}
