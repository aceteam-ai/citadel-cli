package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// buildExposedGateway wires a gateway that proxies /expose/<name> to a test
// backend under the given policy, with MAXIMALLY RESTRICTIVE capability
// permissions (every capability disabled, no node passcode) and the full
// middleware chain built. This is the crux of the #598 test: it proves the
// exposure gate is the SOLE authority for /expose/ routes — a link/org caller
// must reach the backend even though the capability layer would 403/401 every
// other sensitive route. An isolated middleware test would pass while the real
// chain still rejected the caller.
func buildExposedGateway(t *testing.T, name string, policy *ExposePolicy, resolver MeshIdentityResolver, key []byte) (http.Handler, *httptest.Server) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("BACKEND-REACHED:" + r.URL.Path))
	}))
	t.Cleanup(backend.Close)

	gw := NewServer(Config{Port: 0, NodeName: "test-node"})
	// The zero-value Permissions disables every capability and sets no passcode:
	// the harshest possible capability gate.
	gw.SetPermissions(&config.Permissions{})
	gw.SetMeshResolver(resolver)
	gw.SetExposeSigningKey(key)

	if err := gw.Expose(name, backendAddr(t, backend), policy); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	// Register proxy handlers on the mux (Start's loop is not run in tests), then
	// build the full middleware chain.
	for prefix, up := range gw.config.Upstreams {
		gw.registerProxy(prefix, up)
	}
	return gw.BuildHandler(), backend
}

func backendAddr(t *testing.T, s *httptest.Server) string {
	t.Helper()
	// httptest URL is "http://127.0.0.1:PORT"; the upstream wants "host:port".
	return s.Listener.Addr().String()
}

func doGet(h http.Handler, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestExposure_LinkReachesUpstreamThroughFullChain(t *testing.T) {
	key := []byte("test-signing-key-0123456789")
	policy := &ExposePolicy{Visibility: VisibilityLink, TokenEpoch: 1}
	h, _ := buildExposedGateway(t, "frigate", policy, nil, key)

	valid := MintLinkToken(key, "frigate", 1, time.Now().Add(time.Hour))
	if valid == "" {
		t.Fatal("MintLinkToken returned empty for a non-empty key")
	}

	// Valid token -> reaches the backend, despite every capability being disabled.
	if w := doGet(h, "/expose/frigate/dashboard?access_token="+valid); w.Code != http.StatusOK {
		t.Fatalf("valid link token: got %d, want 200 (chain body=%q)", w.Code, w.Body.String())
	}

	// No token -> 401, never reaches the backend.
	if w := doGet(h, "/expose/frigate/dashboard"); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", w.Code)
	}

	// Expired token -> 401.
	expired := MintLinkToken(key, "frigate", 1, time.Now().Add(-time.Minute))
	if w := doGet(h, "/expose/frigate/?access_token="+expired); w.Code != http.StatusUnauthorized {
		t.Errorf("expired token: got %d, want 401", w.Code)
	}

	// Wrong epoch (revoked) -> 401. A token minted at epoch 0 must not open an
	// exposure now at epoch 1.
	revoked := MintLinkToken(key, "frigate", 0, time.Now().Add(time.Hour))
	if w := doGet(h, "/expose/frigate/?access_token="+revoked); w.Code != http.StatusUnauthorized {
		t.Errorf("revoked (old-epoch) token: got %d, want 401", w.Code)
	}

	// A token for a DIFFERENT service name must not open this one.
	other := MintLinkToken(key, "grafana", 1, time.Now().Add(time.Hour))
	if w := doGet(h, "/expose/frigate/?access_token="+other); w.Code != http.StatusUnauthorized {
		t.Errorf("cross-service token: got %d, want 401", w.Code)
	}
}

func TestExposure_LinkCookieCarriesSession(t *testing.T) {
	key := []byte("cookie-key")
	h, _ := buildExposedGateway(t, "frigate", &ExposePolicy{Visibility: VisibilityLink, TokenEpoch: 1}, nil, key)
	tok := MintLinkToken(key, "frigate", 1, time.Now().Add(time.Hour))

	// First request with the explicit token: 200 AND a Set-Cookie carrying the
	// session forward.
	req := httptest.NewRequest(http.MethodGet, "/expose/frigate/?access_token="+tok, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first (token) request: got %d, want 200", w.Code)
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == exposeCookieName("frigate") {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie was set on the token'd request")
	}
	if session.Path != "/expose/frigate/" {
		t.Errorf("cookie path: got %q, want /expose/frigate/", session.Path)
	}

	// Sub-resource fetch carrying ONLY the cookie (no ?access_token=, as a browser
	// would do for /assets/app.js): must be authorized. This is the case a
	// per-request-only design silently 401s.
	sub := httptest.NewRequest(http.MethodGet, "/expose/frigate/assets/app.js", nil)
	sub.AddCookie(session)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, sub)
	if w2.Code != http.StatusOK {
		t.Errorf("cookie-only sub-resource: got %d, want 200", w2.Code)
	}

	// A tampered cookie must fail closed.
	sub2 := httptest.NewRequest(http.MethodGet, "/expose/frigate/assets/app.js", nil)
	sub2.AddCookie(&http.Cookie{Name: exposeCookieName("frigate"), Value: tok + "x"})
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, sub2)
	if w3.Code != http.StatusUnauthorized {
		t.Errorf("tampered cookie: got %d, want 401", w3.Code)
	}
}

func TestExpose_EnablesWebSocketAndStripPrefix(t *testing.T) {
	gw := NewServer(Config{Port: 0})
	if err := gw.Expose("frigate", "127.0.0.1:5000", &ExposePolicy{Visibility: VisibilityOrg}); err != nil {
		t.Fatal(err)
	}
	up := gw.config.Upstreams[ExposeRoutePath("frigate")]
	if up == nil {
		t.Fatal("no upstream registered for the exposed route")
	}
	if !up.WebSocket {
		t.Error("exposed upstream must have WebSocket enabled (live view / event streams)")
	}
	if !up.StripPrefix {
		t.Error("exposed upstream must strip the /expose/<name> prefix")
	}
}

func TestExposure_LinkTokenViaHeader(t *testing.T) {
	key := []byte("hdr-key")
	h, _ := buildExposedGateway(t, "svc", &ExposePolicy{Visibility: VisibilityLink, TokenEpoch: 3}, nil, key)
	tok := MintLinkToken(key, "svc", 3, time.Now().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/expose/svc/", nil)
	req.Header.Set("X-Citadel-Access", tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("header token: got %d, want 200", w.Code)
	}
}

func TestExposure_OrgRequiresSameOwner(t *testing.T) {
	policy := &ExposePolicy{Visibility: VisibilityOrg}

	// Same-owner peer -> allowed.
	sameOwner := &MockMeshResolver{Identity: &MeshPeerIdentity{LoginName: "a@b.co", SameOwner: true}}
	h, _ := buildExposedGateway(t, "svc", policy, sameOwner, nil)
	if w := doGet(h, "/expose/svc/"); w.Code != http.StatusOK {
		t.Errorf("same-owner org: got %d, want 200", w.Code)
	}

	// Different-owner peer -> denied.
	otherOwner := &MockMeshResolver{Identity: &MeshPeerIdentity{LoginName: "x@y.co", SameOwner: false}}
	h2, _ := buildExposedGateway(t, "svc", policy, otherOwner, nil)
	if w := doGet(h2, "/expose/svc/"); w.Code != http.StatusForbidden {
		t.Errorf("different-owner org: got %d, want 403", w.Code)
	}

	// No resolver (e.g. LAN listener) -> fail closed.
	h3, _ := buildExposedGateway(t, "svc", policy, nil, nil)
	if w := doGet(h3, "/expose/svc/"); w.Code != http.StatusForbidden {
		t.Errorf("org without resolver: got %d, want 403", w.Code)
	}
}

func TestExposure_PrivateMatchesCreator(t *testing.T) {
	// Creator match -> allowed.
	creatorPeer := &MockMeshResolver{Identity: &MeshPeerIdentity{LoginName: "owner@team.co", SameOwner: true}}
	h, _ := buildExposedGateway(t, "svc", &ExposePolicy{Visibility: VisibilityPrivate, Creator: "owner@team.co"}, creatorPeer, nil)
	if w := doGet(h, "/expose/svc/"); w.Code != http.StatusOK {
		t.Errorf("creator match: got %d, want 200", w.Code)
	}

	// Different login, same org -> denied (private is not org).
	h2, _ := buildExposedGateway(t, "svc", &ExposePolicy{Visibility: VisibilityPrivate, Creator: "owner@team.co"}, creatorPeer, nil)
	// re-point the same resolver at a different login by rebuilding
	otherPeer := &MockMeshResolver{Identity: &MeshPeerIdentity{LoginName: "someone@team.co", SameOwner: true}}
	h2, _ = buildExposedGateway(t, "svc", &ExposePolicy{Visibility: VisibilityPrivate, Creator: "owner@team.co"}, otherPeer, nil)
	if w := doGet(h2, "/expose/svc/"); w.Code != http.StatusForbidden {
		t.Errorf("non-creator same-org: got %d, want 403", w.Code)
	}

	// Empty creator -> inert, fail closed even for a valid peer.
	h3, _ := buildExposedGateway(t, "svc", &ExposePolicy{Visibility: VisibilityPrivate, Creator: ""}, creatorPeer, nil)
	if w := doGet(h3, "/expose/svc/"); w.Code != http.StatusForbidden {
		t.Errorf("empty-creator private: got %d, want 403", w.Code)
	}
}

func TestExposure_UnregisteredNameFailsClosed(t *testing.T) {
	// Expose "known" but request "ghost": the middleware must 404 the unregistered
	// name rather than fall through to the proxy/root.
	h, _ := buildExposedGateway(t, "known", &ExposePolicy{Visibility: VisibilityOrg}, &MockMeshResolver{Identity: &MeshPeerIdentity{SameOwner: true}}, nil)
	if w := doGet(h, "/expose/ghost/"); w.Code != http.StatusNotFound {
		t.Errorf("unregistered exposure: got %d, want 404", w.Code)
	}
}

func TestExposure_NonExposePathUnaffected(t *testing.T) {
	// A non-/expose/ path must pass straight through the exposure middleware. With
	// all capabilities disabled, /health (category "") is allowed; /terminal
	// (category console, disabled) is 403 — proving exposure middleware does not
	// interfere with the capability gate for builtin routes.
	h, _ := buildExposedGateway(t, "svc", &ExposePolicy{Visibility: VisibilityOrg}, &MockMeshResolver{Identity: &MeshPeerIdentity{SameOwner: true}}, nil)
	if w := doGet(h, "/terminal"); w.Code != http.StatusForbidden {
		t.Errorf("/terminal with console disabled: got %d, want 403", w.Code)
	}
}

func TestMintVerifyLinkToken(t *testing.T) {
	key := []byte("k")
	now := time.Unix(1_000_000, 0)

	tok := MintLinkToken(key, "svc", 2, now.Add(time.Hour))
	if !verifyLinkToken(key, "svc", 2, tok, now) {
		t.Error("fresh token should verify")
	}
	// Expiry boundary.
	if verifyLinkToken(key, "svc", 2, tok, now.Add(2*time.Hour)) {
		t.Error("expired token must not verify")
	}
	// Epoch mismatch.
	if verifyLinkToken(key, "svc", 3, tok, now) {
		t.Error("epoch mismatch must not verify")
	}
	// Name mismatch.
	if verifyLinkToken(key, "other", 2, tok, now) {
		t.Error("name mismatch must not verify")
	}
	// Wrong key.
	if verifyLinkToken([]byte("other-key"), "svc", 2, tok, now) {
		t.Error("wrong key must not verify")
	}
	// Tampered token.
	if verifyLinkToken(key, "svc", 2, tok+"x", now) {
		t.Error("tampered token must not verify")
	}
	// Empty key mints nothing and verifies nothing.
	if MintLinkToken(nil, "svc", 1, now.Add(time.Hour)) != "" {
		t.Error("empty key should mint empty token")
	}
	if verifyLinkToken(nil, "svc", 2, tok, now) {
		t.Error("empty key must not verify")
	}
	// Garbage token.
	if verifyLinkToken(key, "svc", 2, "not-a-token", now) {
		t.Error("garbage token must not verify")
	}
}

func TestExpose_ValidatesName(t *testing.T) {
	gw := NewServer(Config{Port: 0})
	bad := []string{"", "Frigate", "a/b", "a..b", "a b", "-x", "x-"}
	for _, name := range bad {
		if err := gw.Expose(name, "127.0.0.1:5000", &ExposePolicy{Visibility: VisibilityOrg}); err == nil {
			t.Errorf("Expose(%q) should have failed name validation", name)
		}
	}
	// Invalid visibility rejected.
	if err := gw.Expose("frigate", "127.0.0.1:5000", &ExposePolicy{Visibility: "public"}); err == nil {
		t.Error("Expose with invalid visibility should fail")
	}
	// Valid name + visibility accepted.
	if err := gw.Expose("frigate", "127.0.0.1:5000", &ExposePolicy{Visibility: VisibilityOrg}); err != nil {
		t.Errorf("Expose(valid) unexpected error: %v", err)
	}
}

func TestCategoryForPath_ExposeAlwaysAllowed(t *testing.T) {
	for _, p := range []string{"/expose/frigate", "/expose/frigate/", "/expose/x/y/z"} {
		if got := categoryForPath(p); got != "" {
			t.Errorf("categoryForPath(%q) = %q, want \"\" (always-allowed)", p, got)
		}
	}
}

func TestRemoveExposure_FailsClosedAfterRevoke(t *testing.T) {
	key := []byte("k")
	gw := NewServer(Config{Port: 0, NodeName: "n"})
	gw.SetPermissions(&config.Permissions{})
	gw.SetExposeSigningKey(key)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(backend.Close)
	if err := gw.Expose("svc", backend.Listener.Addr().String(), &ExposePolicy{Visibility: VisibilityLink, TokenEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	for prefix, up := range gw.config.Upstreams {
		gw.registerProxy(prefix, up)
	}
	h := gw.BuildHandler()
	tok := MintLinkToken(key, "svc", 1, time.Now().Add(time.Hour))
	if w := doGet(h, "/expose/svc/?access_token="+tok); w.Code != http.StatusOK {
		t.Fatalf("before revoke: got %d, want 200", w.Code)
	}
	gw.RemoveExposure("svc")
	if w := doGet(h, "/expose/svc/?access_token="+tok); w.Code != http.StatusNotFound {
		t.Errorf("after RemoveExposure: got %d, want 404", w.Code)
	}
}
