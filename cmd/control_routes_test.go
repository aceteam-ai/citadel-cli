package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// fakeSink records which registrar the helper hands to which listener.
type fakeSink struct{ plain, control []status.RouteRegistrar }

func (f *fakeSink) AddRouteRegistrar(r status.RouteRegistrar) { f.plain = append(f.plain, r) }
func (f *fakeSink) AddControlRouteRegistrar(r status.RouteRegistrar) {
	f.control = append(f.control, r)
}

// fakeAPI serves "stub" from RegisterRoutes and "real" from RegisterControlRoutes.
type fakeAPI struct{}

func (fakeAPI) RegisterRoutes(mux *http.ServeMux, _ func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("/x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
}

func (fakeAPI) RegisterControlRoutes(mux *http.ServeMux, gate func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("/x", gate(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
}

func serve(reg status.RouteRegistrar, mw func(http.HandlerFunc) http.HandlerFunc) int {
	mux := http.NewServeMux()
	reg(mux, mw)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", nil))
	return w.Code
}

func allow(next http.HandlerFunc) http.HandlerFunc { return next }

// The stubs (RegisterRoutes) must go to the plaintext listener and the real
// handlers (RegisterControlRoutes) to the control listener; swapping them would
// put a control-only API back on the mesh-trusting listener (citadel-cli#1318).
func TestRegisterControlOnlyAPIWiresEachSideToItsListener(t *testing.T) {
	sink := &fakeSink{}
	registerControlOnlyAPI(sink, fakeAPI{}, nil)
	if len(sink.plain) != 1 || len(sink.control) != 1 {
		t.Fatalf("want one registrar per listener, got plain=%d control=%d", len(sink.plain), len(sink.control))
	}
	if code := serve(sink.plain[0], allow); code != http.StatusForbidden {
		t.Fatalf("plaintext listener must get the 403 stub, got %d", code)
	}
	if code := serve(sink.control[0], allow); code != http.StatusOK {
		t.Fatalf("control listener must get the real handler, got %d", code)
	}
}

// The coordinator gate always runs, and an extra gate (an operator permission
// toggle) runs after it.
func TestRegisterControlOnlyAPIAppliesCoordinatorThenExtraGate(t *testing.T) {
	refuse := func(http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }
	}
	sink := &fakeSink{}
	registerControlOnlyAPI(sink, fakeAPI{}, permissionGate("test", func() bool { return true }))
	if code := serve(sink.control[0], refuse); code != http.StatusUnauthorized {
		t.Fatalf("coordinator gate not applied: got %d", code)
	}
	off := &fakeSink{}
	registerControlOnlyAPI(off, fakeAPI{}, permissionGate("test", func() bool { return false }))
	if code := serve(off.control[0], allow); code != http.StatusForbidden {
		t.Fatalf("permission toggle off must refuse, got %d", code)
	}
	on := &fakeSink{}
	registerControlOnlyAPI(on, fakeAPI{}, permissionGate("test", func() bool { return true }))
	if code := serve(on.control[0], allow); code != http.StatusOK {
		t.Fatalf("permission toggle on must admit, got %d", code)
	}
}
