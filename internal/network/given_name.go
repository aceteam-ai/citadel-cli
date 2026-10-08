package network

import "strings"

// citadelGivenNameSuffix distinguishes a citadel-registered node's Headscale
// given-name from the host's own system tailscaled node, which registers under
// the bare hostname. Without it, a citadel FRESH registration on a host that
// also runs tailscaled collides on the base name and climbs -1/-2/... forever,
// because the host's tailscale node is a permanent collision the control-plane
// GC cannot remove (#1235). Applied only on a fresh register; a reattach keeps
// the existing name via the preserved machine key, so this never renames an
// already-registered node in place.
const citadelGivenNameSuffix = "-citadel"

// CitadelGivenName returns the given-name a FRESH citadel registration should
// present to the coordination server, distinct from the host's own tailscaled
// node. It appends "-citadel" unless base is empty or already carries it.
func CitadelGivenName(base string) string {
	if base == "" || strings.HasSuffix(base, citadelGivenNameSuffix) {
		return base
	}
	return base + citadelGivenNameSuffix
}
