package cmd

import (
	"fmt"
	"io"
	"net"
	"os"
)

// defaultGatewayBind is the gateway's default listen host: loopback only. The
// mesh listener is attached separately (network.ListenVPN), so peers reach the
// gateway over the mesh without any all-interfaces bind.
const defaultGatewayBind = "127.0.0.1"

// gatewayBindIsLoopback reports whether host binds loopback only. It decides
// whether an operator-supplied --gateway-bind/--bind is the explicit LAN
// opt-in that must be announced.
func gatewayBindIsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// gatewayLANBindMessage is the loud notice for a gateway bind reachable beyond
// this host. It states plainly what a LAN caller can reach.
func gatewayLANBindMessage(flag, host string) string {
	return fmt.Sprintf("WARNING: gateway bound to %q via %s: it is reachable beyond this host (LAN). Routes the gateway proxies to loopback upstreams (status, services, screenshot and other routes) are reachable by LAN callers; only /v1/* requires a verified mesh identity. The default is loopback plus the mesh listener only.", host, flag)
}

// warnGatewayLANBindTo writes the notice to w when host is not loopback and
// reports whether it did. Split from warnGatewayLANBind so tests can capture it.
func warnGatewayLANBindTo(w io.Writer, flag, host string) bool {
	if gatewayBindIsLoopback(host) {
		return false
	}
	fmt.Fprintln(w, "   - "+gatewayLANBindMessage(flag, host))
	return true
}

// warnGatewayLANBind announces an operator opt-in to a LAN-reachable gateway
// bind on stderr and in the node log. It returns whether it warned.
func warnGatewayLANBind(flag, host string) bool {
	if !warnGatewayLANBindTo(os.Stderr, flag, host) {
		return false
	}
	Log("%s", gatewayLANBindMessage(flag, host))
	return true
}
