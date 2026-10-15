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
// It must also arrive on that address (the connection's local address), which
// rules out a LAN connection forging the mesh source.
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
	// Presence, not value: an empty or duplicated forwarding header still marks
	// a forwarded request.
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP"} {
		if len(r.Header.Values(h)) > 0 {
			return false
		}
	}
	if s.selfMeshIPs == nil {
		return false
	}
	src, ok := addrIP(r.RemoteAddr)
	if !ok || src.IsLoopback() || src.IsUnspecified() {
		return false
	}
	// The connection must also have ARRIVED on the node's own mesh address.
	// The status server binds every interface, and the WireGuard source check
	// only holds on the mesh listener; a LAN connection carrying the mesh IP as
	// its source arrives on the LAN address and is refused here.
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if la == nil {
		return false
	}
	dst, ok := addrIP(la.String())
	if !ok {
		return false
	}
	srcSelf, dstSelf := false, false
	for _, self := range s.selfMeshIPs() {
		if !self.IsValid() || self.IsLoopback() || self.IsUnspecified() {
			continue
		}
		self = self.Unmap()
		srcSelf = srcSelf || self == src
		dstSelf = dstSelf || self == dst
	}
	return srcSelf && dstSelf
}

// addrIP parses the IP out of a "host:port" (or bare host) address string.
func addrIP(hostport string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}
