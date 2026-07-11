package cmd

import (
	"fmt"
	"strings"

	"github.com/aceteam-ai/citadel-cli/services"
)

// gatewayVPNBindErrorMessage renders the startup error shown when the HTTPS
// gateway cannot bind its mesh (tsnet) listener.
//
// This must be LOUD (#504): the gateway is the ONLY mesh entry point for
// /vnc, /terminal, and /modules/*, so losing the bind means those routes are
// unreachable over the mesh while the node still reports healthy in every
// heartbeat. The old one-line "LAN-only" warning hid exactly that on every
// node in the fleet after the mTLS control listener (#5028) started claiming
// the same mesh :8443 first.
//
// tsnet reports a same-process double-bind as "listener already open" — that
// is a collision between two citadel-owned listeners (a config/regression
// bug, not an environment flake), so the message names the port and the
// known culprit knobs instead of leaving the operator to guess.
func gatewayVPNBindErrorMessage(port int, err error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "❌ GATEWAY MESH LISTENER FAILED: could not bind mesh port %d: %v\n", port, err)
	b.WriteString("   Mesh routes /vnc, /terminal, and /modules/* are UNREACHABLE over the VPN.\n")
	b.WriteString("   The gateway continues on LAN only; platform features that dial this node over the mesh (desktop, WhatsApp bridge, provisioned modules) will fail.\n")
	if strings.Contains(err.Error(), "listener already open") {
		fmt.Fprintf(&b, "   Another citadel listener already owns mesh port %d.", port)
		fmt.Fprintf(&b, " Check --gateway-port against the mTLS control listener (CITADEL_CONTROL_PORT, default %d) and restart with distinct ports.", services.ControlMTLSPort)
	}
	return b.String()
}
