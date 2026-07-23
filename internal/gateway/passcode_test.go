package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// TestPermissionMiddleware_SensitivePasscodeGate verifies the aceteam#6524
// gateway behavior for a sensitive surface (/api/screenshot -> desktop):
//   - disabled: blocked (403) regardless of passcode
//   - enabled + no passcode set: fails closed (401)
//   - enabled + wrong passcode: 401
//   - enabled + correct passcode: passes through
func TestPermissionMiddleware_SensitivePasscodeGate(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	newGateway := func(p *config.Permissions) http.Handler {
		s := NewServer(Config{NodeName: "test-node"})
		s.SetPermissions(p)
		return s.permissionMiddleware(next)
	}

	call := func(h http.Handler, passcode string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
		if passcode != "" {
			req.Header.Set("X-Citadel-Passcode", passcode)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	t.Run("disabled surface is blocked", func(t *testing.T) {
		p := config.DefaultPermissions() // desktop=false
		_ = p.SetPasscode("1379")
		if code := call(newGateway(p), "1379"); code != http.StatusForbidden {
			t.Errorf("disabled desktop should be 403, got %d", code)
		}
	})

	t.Run("enabled but no passcode fails closed", func(t *testing.T) {
		p := config.DefaultPermissions()
		p.Desktop = true // enabled, but no passcode
		if code := call(newGateway(p), ""); code != http.StatusUnauthorized {
			t.Errorf("enabled-no-passcode should be 401, got %d", code)
		}
	})

	t.Run("enabled + wrong passcode is denied", func(t *testing.T) {
		p := config.DefaultPermissions()
		p.Desktop = true
		_ = p.SetPasscode("1379")
		if code := call(newGateway(p), "0000"); code != http.StatusUnauthorized {
			t.Errorf("wrong passcode should be 401, got %d", code)
		}
	})

	t.Run("enabled + correct passcode passes", func(t *testing.T) {
		p := config.DefaultPermissions()
		p.Desktop = true
		_ = p.SetPasscode("1379")
		if code := call(newGateway(p), "1379"); code != http.StatusOK {
			t.Errorf("correct passcode should pass (200), got %d", code)
		}
	})
}

// TestPermissionMiddleware_NonSensitiveNoPasscode confirms a non-sensitive,
// default-on surface (services) is NOT passcode-gated: it passes with no
// passcode, so the passcode change does not regress ordinary operation.
func TestPermissionMiddleware_NonSensitiveNoPasscode(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	p := config.DefaultPermissions() // services=true, no passcode
	s := NewServer(Config{NodeName: "test-node"})
	s.SetPermissions(p)
	h := s.permissionMiddleware(next)

	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("non-sensitive services route should pass without a passcode, got %d", w.Code)
	}
}
