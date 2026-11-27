package terminal

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

func TestLivePolicyTerminalAdmission(t *testing.T) {
	for _, state := range []string{"disabled", "unavailable", "nil"} {
		t.Run(state, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.PermissionSource = func() (*config.Permissions, error) {
				switch state {
				case "unavailable":
					return nil, errors.New("fixture")
				case "nil":
					return nil, nil
				}
				return &config.Permissions{}, nil
			}
			s := NewServer(cfg, NewMockTokenValidator())
			w := httptest.NewRecorder()
			s.handleWebSocket(w, httptest.NewRequest("GET", "/terminal", nil))
			if w.Code != http.StatusForbidden {
				t.Fatal(w.Code)
			}
			if s.SessionCount() != 0 {
				t.Fatal("created session")
			}
		})
	}
}

func TestLivePolicyTerminalPasscodeReload(t *testing.T) {
	dir := t.TempDir()
	p := &config.Permissions{Console: true}
	if err := p.SetPasscode("fixture-old"); err != nil {
		t.Fatal(err)
	}
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.OrgID = "fixture-org"
	cfg.MaxConnections = 0
	cfg.PermissionSource = func() (*config.Permissions, error) { return config.LoadPermissionsStrict(dir) }
	auth := NewMockTokenValidator()
	auth.AddValidToken("fixture-token", &TokenInfo{UserID: "fixture-user", OrgID: cfg.OrgID})
	s := NewServer(cfg, auth)
	s.SetSilent()
	call := func(pin string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/terminal?token=fixture-token", nil)
		r.Header.Set("X-Citadel-Passcode", pin)
		s.handleWebSocket(w, r)
		return w.Code
	}
	// A correct passcode reaches the configured capacity limit, without a PTY.
	if got := call("fixture-old"); got != http.StatusServiceUnavailable {
		t.Fatal(got)
	}
	if err := p.SetPasscode("fixture-new"); err != nil {
		t.Fatal(err)
	}
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	if got := call("fixture-old"); got != http.StatusUnauthorized {
		t.Fatal(got)
	}
	if got := call("fixture-new"); got != http.StatusServiceUnavailable {
		t.Fatal(got)
	}
	p.Console = false
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	if got := call("fixture-new"); got != http.StatusForbidden {
		t.Fatal(got)
	}
	p.Console = true
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	if got := call("fixture-new"); got != http.StatusServiceUnavailable {
		t.Fatal(got)
	}
}
