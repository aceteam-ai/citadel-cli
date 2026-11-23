package cmd

import (
	"net/http"

	"github.com/aceteam-ai/citadel-cli/internal/config"
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

// registerProvisionControlAPI wires the provisioning API onto the control
// listener behind the operator's `provision` opt-out
// (config.Permissions.Provision, default on), read strictly per request from
// permissionsDir(), so toggling it needs no restart and a damaged file refuses.
// runWork passes nodePermissionsDir; tests pass a temp dir.
func registerProvisionControlAPI(sink routeRegistrarSink, api controlOnlyAPI, permissionsDir func() string) {
	registerControlOnlyAPI(sink, api, permissionGate("provisioning", func() bool { return provisionEnabledIn(permissionsDir()) }))
}

// registerRAGControlAPI wires the /rag API, which reads node content, onto the
// control listener behind the operator's live Files permission, read strictly
// per request from permissionsDir(). runWork passes nodePermissionsDir.
func registerRAGControlAPI(sink routeRegistrarSink, api controlOnlyAPI, permissionsDir func() string) {
	registerControlOnlyAPI(sink, api, permissionGate("files", func() bool { return filesEnabledIn(permissionsDir()) }))
}

// provisionEnabledIn reports the Provision permission. Any load error refuses.
func provisionEnabledIn(permissionsDir string) bool {
	p, err := config.LoadPermissionsStrict(permissionsDir)
	return err == nil && p.Provision
}

// filesEnabledIn reports the Files permission. Any load error refuses.
func filesEnabledIn(permissionsDir string) bool {
	p, err := config.LoadPermissionsStrict(permissionsDir)
	return err == nil && p.Files
}

// nodeSSHDeployAllowed backs status.ServerConfig.SSHDeployAllowed: the
// control listener's SSH-key deployment honors the operator's live SSH
// permission.
func nodeSSHDeployAllowed() bool {
	return sshDeployAllowedIn(nodePermissionsDir())
}

// sshDeployAllowedIn is nodeSSHDeployAllowed's pure core. A missing file keeps
// the default (SSH on); any other read error or a file that does not parse
// refuses.
//
// TODO(#1882): also refuse while the setup posture is active
// (config.SetupPostureActive) once that lands on main.
func sshDeployAllowedIn(permissionsDir string) bool {
	p, err := config.LoadPermissionsStrict(permissionsDir)
	return err == nil && p.SSH
}

// permissionGate refuses with a fixed 403 while enabled() reports false.
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
