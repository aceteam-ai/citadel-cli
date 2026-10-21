package terminal

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

// TestPasscodeGate_LegacyPasscodeLockout: the terminal WebSocket passcode gate,
// wired the way cmd wires it (Permissions.VerifyPasscode), reaches the
// node-wide lockout on a legacy-passcode node (#1398). The handler is called
// directly; no listener is opened.
func TestPasscodeGate_LegacyPasscodeLockout(t *testing.T) {
	lockout := nodevault.OpenLegacyPasscodeLockout(t.TempDir())
	orig := config.LegacyPasscodeLockout
	config.LegacyPasscodeLockout = func() config.PasscodeAttemptLimiter { return lockout }
	t.Cleanup(func() { config.LegacyPasscodeLockout = orig })

	perms := config.DefaultPermissions()
	if err := perms.SetPasscode("correct-pin"); err != nil {
		t.Fatal(err)
	}
	auth := NewMockTokenValidator()
	auth.AddValidToken("tok-1", &TokenInfo{UserID: "alice", OrgID: "org-1"})
	s := NewServer(&Config{
		Host:                "127.0.0.1",
		MaxConnections:      10,
		IdleTimeout:         time.Minute,
		OrgID:               "org-1",
		Shell:               "/bin/sh",
		RateLimitRPS:        1000,
		RateLimitBurst:      1000,
		PasscodeVerifier:    perms.VerifyPasscode,
		PasscodeHasPasscode: perms.HasPasscode,
	}, auth)

	call := func(passcode string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/terminal?token=tok-1", nil)
		if passcode != "" {
			req.Header.Set("X-Citadel-Passcode", passcode)
		}
		rec := httptest.NewRecorder()
		s.handleWebSocket(rec, req)
		return rec.Code, rec.Body.String()
	}

	if code, _ := call(""); code != http.StatusUnauthorized {
		t.Fatalf("empty passcode: %d", code)
	}
	if st := lockout.Status(); st.FailedAttempts != 0 {
		t.Fatalf("an empty passcode was counted: %+v", st)
	}
	for i := 0; i < 5; i++ {
		if code, _ := call("wrong-pin"); code != http.StatusUnauthorized {
			t.Fatalf("wrong passcode %d: %d", i, code)
		}
	}
	// During a lockout the correct passcode gets the same reject as a wrong one.
	code, body := call("correct-pin")
	if code != http.StatusUnauthorized {
		t.Fatalf("correct passcode during lockout: got %d, want 401", code)
	}
	_, wrongBody := call("wrong-pin")
	if body != wrongBody {
		t.Fatalf("lockout reject %q differs from mismatch reject %q", body, wrongBody)
	}
	if st := lockout.Status(); !st.LockedOut || st.FailedAttempts != 5 {
		t.Fatalf("lockout state: %+v", st)
	}
}
