package cmd

import (
	"net/http"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// controlOnlyAPI is a feature HTTP API served only on the mTLS control
// listener (citadel-cli#1318): RegisterRoutes installs the plaintext 403 stubs
// and RegisterControlRoutes installs the real handlers.
type controlOnlyAPI interface {
	RegisterRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc)
	RegisterControlRoutes(mux *http.ServeMux, requireCoordinator func(http.HandlerFunc) http.HandlerFunc)
}

// routeRegistrarSink is the subset of *status.Server the helper needs, so the
// wiring is testable without starting a server.
type routeRegistrarSink interface {
	AddRouteRegistrar(status.RouteRegistrar)
	AddControlRouteRegistrar(status.RouteRegistrar)
}

// registerControlOnlyAPI is the single place a control-only API is wired: the
// stubs go on the plaintext listener and the real handlers on the control
// listener. extraGate, when non-nil, runs after requireCoordinator on every
// control route (for example an operator permission toggle).
func registerControlOnlyAPI(sink routeRegistrarSink, api controlOnlyAPI, extraGate func(http.HandlerFunc) http.HandlerFunc) {
	sink.AddRouteRegistrar(api.RegisterRoutes)
	sink.AddControlRouteRegistrar(func(mux *http.ServeMux, requireCoordinator func(http.HandlerFunc) http.HandlerFunc) {
		gate := requireCoordinator
		if extraGate != nil {
			gate = func(next http.HandlerFunc) http.HandlerFunc { return requireCoordinator(extraGate(next)) }
		}
		api.RegisterControlRoutes(mux, gate)
	})
}

// requireProvisionPermission keeps the operator's `provision` opt-out
// (config.Permissions.Provision, default on) effective on the control
// listener. It is read per request, so toggling it needs no restart.
func requireProvisionPermission(next http.HandlerFunc) http.HandlerFunc {
	return permissionGate("provisioning", func() bool { return loadNodePermissions().Provision })(next)
}

// permissionGate refuses with 403 while enabled() reports false. The pure core
// of requireProvisionPermission, so tests never read the live node's
// permissions file.
func permissionGate(surface string, enabled func() bool) func(http.HandlerFunc) http.HandlerFunc {
	msg := `{"error":"the ` + surface + ` API is disabled on this node by its permission toggle"}`
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !enabled() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(msg))
				return
			}
			next(w, r)
		}
	}
}
