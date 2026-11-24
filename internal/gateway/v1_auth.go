// internal/gateway/v1_auth.go
//
// Authentication for the OpenAI-compatible /v1/* surface (chat completions,
// completions, models, embeddings). These routes reach local inference engines,
// so they must not answer an arbitrary caller. categoryForPath still reports no
// capability for them (they are not a permission category); v1AuthMiddleware is
// their sole caller gate, mirroring how /expose/, /cache/ and /borrow/ are gated
// by their own identity checks.
//
// # The gate, in order (fail closed at every step)
//
//  1. Same-host callers (the connection's RemoteAddr is a loopback address) are
//     allowed. Same-host processes are trusted exactly as they are for the
//     status server's loopback routes: anything that can open a loopback socket
//     already runs as a local user of the node. The gateway sits behind no
//     reverse proxy of its own, so RemoteAddr is the real peer; a forwarding
//     header is never consulted.
//  2. Every other caller must resolve, through the injected MeshIdentityResolver
//     (production: network.WhoIsPeer), to a verified mesh peer with a stable
//     device id and owner id, and be either same-owner or a login on the
//     operator's platform-infra allowlist (serviceborrow.Decide, the same
//     decision the borrow routes use). The source address range is never a
//     credential: a LAN caller, a mesh address the control plane cannot vouch
//     for, a nil resolver and a resolver error are all refused.
package gateway

import (
	"net"
	"net/http"
	"net/netip"
	"path"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/serviceborrow"
)

// V1AllowedLogins returns the operator-approved platform-infra logins that may
// call /v1/* in addition to same-owner peers. It is consulted only for a peer
// that is not same-owner. An error degrades to same-owner-only (never wider).
type V1AllowedLogins func() (map[string]bool, error)

// SetV1AllowedLogins wires the platform-infra allowlist for /v1/* callers. Nil
// (the default) means same-owner peers only. Safe to call before Start.
func (s *Server) SetV1AllowedLogins(f V1AllowedLogins) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.v1Allowed = f
}

// isV1Path reports whether a request path addresses the /v1 surface. The path is
// cleaned first so dot segments cannot dodge the gate.
func isV1Path(p string) bool {
	c := path.Clean("/" + p)
	return c == "/v1" || strings.HasPrefix(c, "/v1/")
}

// isLoopbackRemote reports whether remoteAddr ("ip:port") is a loopback address.
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.Unmap().IsLoopback()
}

// v1AuthMiddleware gates /v1/* requests; every other path passes through.
func (s *Server) v1AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isV1Path(r.URL.Path) || isLoopbackRemote(r.RemoteAddr) {
			next.ServeHTTP(w, r)
			return
		}

		s.mu.RLock()
		resolver := s.meshResolver
		allowedFn := s.v1Allowed
		s.mu.RUnlock()

		mid, ok := resolvePeer(resolver, r)
		if !ok {
			exposeDeny(w, http.StatusForbidden, "verified mesh peer required")
			return
		}
		pid := serviceborrow.PeerIdentity{
			NodeName:  mid.NodeName,
			LoginName: mid.LoginName,
			StableID:  mid.StableID,
			OwnerID:   mid.OwnerID,
			SameOwner: mid.SameOwner,
		}
		var allowed map[string]bool
		// The allowlist only matters for a peer that is not same-owner; skip
		// the file read otherwise. A loader error leaves allowed nil, which
		// reduces to same-owner-only.
		if !(pid.SameOwner && pid.OwnerID != "") && allowedFn != nil {
			if a, err := allowedFn(); err == nil {
				allowed = a
			}
		}
		if err := serviceborrow.Decide(&pid, allowed); err != nil {
			exposeDeny(w, http.StatusForbidden, "caller not authorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}
