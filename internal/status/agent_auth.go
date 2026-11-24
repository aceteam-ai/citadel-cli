package status

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// PeerVerifier reports whether the mesh peer behind remoteAddr ("ip:port") is a
// verified device that may call the read-only /agent/* routes. The production
// implementation (cmd.meshPeerVerifier) resolves the address through the
// coordination server's WhoIs and requires a same-owner peer with a stable
// device and owner id. Any error must be reported as false.
type PeerVerifier func(ctx context.Context, remoteAddr string) bool

// hasForwardingHeader reports whether the request carries a proxy header.
// Presence, not value: an empty or duplicated header still marks a forwarded
// request.
func hasForwardingHeader(r *http.Request) bool {
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP"} {
		if len(r.Header.Values(h)) > 0 {
			return true
		}
	}
	return false
}

// isLoopbackCaller reports a direct loopback connection with no forwarding
// header. A loopback source cannot be forged from the LAN or the mesh. A
// forwarded request is refused because the exposure relay dials loopback on
// behalf of remote visitors.
func isLoopbackCaller(r *http.Request) bool {
	if hasForwardingHeader(r) {
		return false
	}
	src, ok := addrIP(r.RemoteAddr)
	return ok && src.IsLoopback()
}

// requireAgentCaller gates the read-only /agent/* routes on the plaintext
// listener. Mesh origin alone is not an identity: it admits a direct loopback
// caller, a mesh peer the PeerVerifier vouches for, or a valid org token, and
// refuses everyone else (including a CGNAT source with no verified identity).
func (s *Server) requireAgentCaller(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isLoopbackCaller(r) {
			next(w, r)
			return
		}
		if !hasForwardingHeader(r) && isVPNOrigin(r) && s.peerVerifier != nil && s.peerVerifier(r.Context(), r.RemoteAddr) {
			next(w, r)
			return
		}
		if s.tokenValidator != nil {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token != "" && token != r.Header.Get("Authorization") {
				if _, err := s.tokenValidator.ValidateToken(token, s.orgID); err == nil {
					next(w, r)
					return
				}
			}
		}
		writeJSONError(w, "a verified caller is required: same-owner mesh identity, loopback, or a valid token", http.StatusForbidden)
	}
}

// agentMovedToControl is the fail-closed stub left on the plaintext listener
// for /agent/* routes that read node content or mutate state. Only a direct
// loopback caller (the operator on the node) is served; everyone else, mesh
// peers included, is pointed at the mTLS control listener.
func (s *Server) agentMovedToControl(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isLoopbackCaller(r) {
			next(w, r)
			return
		}
		msg := "this endpoint requires a coordinator mTLS client certificate on the control listener"
		if s.controlEnabled() {
			msg = fmt.Sprintf("%s (port %d)", msg, s.controlPort)
		}
		writeJSONError(w, msg, http.StatusForbidden)
	}
}
