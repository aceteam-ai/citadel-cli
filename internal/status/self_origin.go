package status

import (
	"net"
	"net/http"
	"net/netip"
)

// requireSelfOrigin admits a request only when it comes from this node itself,
// identified by a source address equal to one of the node's own AceTeam
// Network addresses (citadel-cli#1318). Another mesh peer cannot present that
// source address: WireGuard only accepts a peer's packets from the peer's own
// allowed IPs.
//
// Loopback is deliberately NOT accepted. The node gateway and the platform
// exposure relay both dial loopback (the relay as raw TCP, with no forwarding
// header), so a loopback source does not prove a local caller. A request
// carrying a forwarding header is refused too, as defense in depth. Unknown
// self addresses refuse (fail closed).
func (s *Server) requireSelfOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isSelfOrigin(r) {
			writeJSONError(w, "this endpoint only accepts requests from the node itself (run `citadel service expose` on the node)", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) isSelfOrigin(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" || r.Header.Get("X-Real-IP") != "" {
		return false
	}
	if s.selfMeshIPs == nil {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	src, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	src = src.Unmap()
	if src.IsLoopback() || src.IsUnspecified() {
		return false
	}
	for _, self := range s.selfMeshIPs() {
		if self.IsValid() && !self.IsLoopback() && !self.IsUnspecified() && self.Unmap() == src {
			return true
		}
	}
	return false
}
