package cmd

import (
	"fmt"
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

// warnGatewayLANBind prints a loud notice when the operator opted in to a
// gateway bind that is reachable beyond this host. It returns whether it warned.
func warnGatewayLANBind(flag, host string) bool {
	if gatewayBindIsLoopback(host) {
		return false
	}
	msg := fmt.Sprintf("WARNING: gateway bound to %q via %s: it is reachable beyond this host (LAN). /v1/* callers still need a verified mesh identity, but the default is loopback plus the mesh listener only.", host, flag)
	Log("%s", msg)
	fmt.Fprintln(os.Stderr, "   - "+msg)
	return true
}
