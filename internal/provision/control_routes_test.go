package provision

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The plaintext status listener trusts mesh origin, so provisioning (which runs
// a caller-supplied image and command) must not be served there at all, even
// behind a permissive auth middleware (citadel-cli#1318).
func TestPlaintextServesNoProvisioningRoutes(t *testing.T) {
	handler, mgr, backend := setupTestHandler(t)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux, noAuthMiddleware)

	for _, rq := range []struct{ method, path, body string }{
		{http.MethodPost, "/provision/create", `{"name":"evil","type":"docker","image":"alpine","command":["sh","-c","id"]}`},
		{http.MethodGet, "/provision/list", ""},
		{http.MethodGet, "/provision/some-id", ""},
		{http.MethodDelete, "/provision/some-id", ""},
		{http.MethodGet, "/provision", ""},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(rq.method, rq.path, strings.NewReader(rq.body)))
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s %s on the plaintext listener: want 403, got %d: %s", rq.method, rq.path, w.Code, w.Body.String())
		}
	}
	if backend.createCalls != 0 {
		t.Fatalf("plaintext request reached the backend: %d create calls", backend.createCalls)
	}
	if res := mgr.List(); len(res) != 0 {
		t.Fatalf("plaintext request recorded resources: %v", res)
	}
}

// Every control route is wrapped by the coordinator gate it is handed; a
// refusing gate means no handler runs.
func TestControlProvisioningRoutesGatedByCoordinator(t *testing.T) {
	handler, _, backend := setupTestHandler(t)
	mux := http.NewServeMux()
	refuse := func(http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }
	}
	handler.RegisterControlRoutes(mux, refuse)
	for _, p := range []string{"/provision/create", "/provision/list", "/provision/some-id"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{"name":"x","type":"docker","image":"alpine"}`)))
		if w.Code != http.StatusForbidden {
			t.Fatalf("control %s not gated: got %d", p, w.Code)
		}
	}
	if backend.createCalls != 0 {
		t.Fatalf("gated request reached the backend: %d create calls", backend.createCalls)
	}
}
