package status

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
)

// TestRequireAuth_LegacyPasscodeLockout: the status server's desktop passcode
// gate, wired the way cmd wires it (Permissions.VerifyPasscode), reaches the
// node-wide lockout on a legacy-passcode node (#1398). The handler is called
// directly; no listener is opened.
func TestRequireAuth_LegacyPasscodeLockout(t *testing.T) {
	lockout := nodevault.OpenLegacyPasscodeLockout(t.TempDir())
	orig := config.LegacyPasscodeLockout
	config.LegacyPasscodeLockout = func() config.PasscodeAttemptLimiter { return lockout }
	t.Cleanup(func() { config.LegacyPasscodeLockout = orig })

	perms := config.DefaultPermissions()
	if err := perms.SetPasscode("1379"); err != nil {
		t.Fatal(err)
	}
	auth := terminal.NewMockTokenValidator()
	auth.AddValidToken("tok-1", &terminal.TokenInfo{UserID: "alice", OrgID: "org-1"})
	s := NewServer(ServerConfig{TokenValidator: auth, OrgID: "org-1", PasscodeVerifier: perms.VerifyPasscode}, nil)
	h := s.requireAuth(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	call := func(passcode string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
		req.Header.Set("Authorization", "Bearer tok-1")
		if passcode != "" {
			req.Header.Set("X-Citadel-Passcode", passcode)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}

	if code := call(""); code != http.StatusUnauthorized {
		t.Fatalf("empty passcode: %d", code)
	}
	if st := lockout.Status(); st.FailedAttempts != 0 {
		t.Fatalf("an empty passcode was counted: %+v", st)
	}
	if code := call("1379"); code != http.StatusOK {
		t.Fatalf("correct passcode before lockout: %d", code)
	}
	for i := 0; i < 5; i++ {
		if code := call("0000"); code != http.StatusUnauthorized {
			t.Fatalf("wrong passcode %d: %d", i, code)
		}
	}
	if code := call("1379"); code != http.StatusUnauthorized {
		t.Fatalf("correct passcode during lockout: got %d, want 401", code)
	}
	if st := lockout.Status(); !st.LockedOut || st.FailedAttempts != 5 {
		t.Fatalf("lockout state: %+v", st)
	}
}
