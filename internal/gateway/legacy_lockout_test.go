package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

// useLegacyLockout wires the node-wide legacy passcode lockout (#1398) to a
// per-test state directory, as package cmd wires it in production. No vault
// hooks are set, so VerifyPasscode takes the legacy bcrypt path. Not parallel.
func useLegacyLockout(t *testing.T) *nodevault.Lockout {
	t.Helper()
	lockout := nodevault.OpenLockout(t.TempDir())
	orig := config.LegacyPasscodeLockout
	config.LegacyPasscodeLockout = func() config.PasscodeAttemptLimiter { return lockout }
	t.Cleanup(func() { config.LegacyPasscodeLockout = orig })
	return lockout
}

const lockoutFreeAttempts = 5 // nodevault.maxFreeAttempts

// TestPermissionMiddleware_LegacyPasscodeLockout: the gateway's sensitive-route
// passcode gate reaches the node-wide lockout on a legacy-passcode node. Wrong
// guesses are counted, a lockout denies even the correct passcode with the same
// 401 a wrong passcode gets, and an empty passcode is not counted.
func TestPermissionMiddleware_LegacyPasscodeLockout(t *testing.T) {
	lockout := useLegacyLockout(t)
	p := config.DefaultPermissions()
	p.Desktop = true
	if err := p.SetPasscode("1379"); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{NodeName: "test-node"})
	s.SetPermissions(p)
	h := s.permissionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	call := func(passcode string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
		if passcode != "" {
			req.Header.Set("X-Citadel-Passcode", passcode)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	for i := 0; i < 3; i++ {
		if code := call(""); code != http.StatusUnauthorized {
			t.Fatalf("empty passcode: %d", code)
		}
	}
	if st := lockout.Status(); st.FailedAttempts != 0 {
		t.Fatalf("empty passcodes were counted: %+v", st)
	}
	for i := 0; i < lockoutFreeAttempts; i++ {
		if code := call("0000"); code != http.StatusUnauthorized {
			t.Fatalf("wrong passcode %d: %d", i, code)
		}
	}
	if code := call("1379"); code != http.StatusUnauthorized {
		t.Fatalf("correct passcode during lockout: got %d, want 401", code)
	}
	if st := lockout.Status(); !st.LockedOut || st.FailedAttempts != lockoutFreeAttempts {
		t.Fatalf("lockout state: %+v", st)
	}
}

// TestPermissionMiddleware_ProxiedWrongPasscodeCountsOnce: a sensitive request
// is checked by the gateway and, after it passes, by the downstream server's
// own verifier (the status server's requireAuth, the terminal server). A wrong
// passcode is refused at the gateway and never reaches the downstream verifier,
// so one presented passcode records exactly one failure.
func TestPermissionMiddleware_ProxiedWrongPasscodeCountsOnce(t *testing.T) {
	lockout := useLegacyLockout(t)
	p := config.DefaultPermissions()
	p.Desktop = true
	if err := p.SetPasscode("1379"); err != nil {
		t.Fatal(err)
	}
	downstreamChecks := 0
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downstreamChecks++
		if !p.VerifyPasscode(passcodeFromRequest(r)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	s := NewServer(Config{NodeName: "test-node"})
	s.SetPermissions(p)
	h := s.permissionMiddleware(downstream)

	req := httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
	req.Header.Set("X-Citadel-Passcode", "0000")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || downstreamChecks != 0 {
		t.Fatalf("wrong passcode: code %d, downstream checks %d", w.Code, downstreamChecks)
	}
	if st := lockout.Status(); st.FailedAttempts != 1 {
		t.Fatalf("one wrong passcode recorded %d failures", st.FailedAttempts)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
	req.Header.Set("X-Citadel-Passcode", "1379")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || downstreamChecks != 1 {
		t.Fatalf("correct passcode: code %d, downstream checks %d", w.Code, downstreamChecks)
	}
	if st := lockout.Status(); st.FailedAttempts != 1 {
		t.Fatalf("a correct passcode changed the failure count: %+v", st)
	}
}

// TestCacheServe_LegacyPasscodeLockout: the cache gate reaches the node-wide
// lockout on a legacy-passcode node, and requests refused for identity before
// the passcode check are never counted.
func TestCacheServe_LegacyPasscodeLockout(t *testing.T) {
	lockout := useLegacyLockout(t)
	provider, _ := buildCacheFixture(t)
	perms := authorizedPerms(t)

	// Identity is checked first: a wrong passcode from a non-peer is not counted.
	gwNoPeer := newCacheGateway(provider, nil, perms)
	rec := httptest.NewRecorder()
	gwNoPeer.mux.ServeHTTP(rec, cacheReq(http.MethodGet, cacheIndexPath, "wrong"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-peer: %d", rec.Code)
	}
	if st := lockout.Status(); st.FailedAttempts != 0 {
		t.Fatalf("an identity-refused request was counted: %+v", st)
	}

	gw := newCacheGateway(provider, okResolver(), perms)
	for i := 0; i < lockoutFreeAttempts; i++ {
		rec := httptest.NewRecorder()
		gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, cacheIndexPath, "wrong"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong passcode %d: %d", i, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, cacheIndexPath, testPasscode))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("correct passcode during lockout: got %d, want 401", rec.Code)
	}
	if st := lockout.Status(); !st.LockedOut || st.FailedAttempts != lockoutFreeAttempts {
		t.Fatalf("lockout state: %+v", st)
	}
}
