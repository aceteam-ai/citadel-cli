package egressrelay

import (
	"fmt"
	"net"
	"strings"
)

// cgnatBlock is 100.64.0.0/10, the Carrier-Grade NAT range citadel's own
// tsnet mesh IPs are assigned from (see internal/network's TailscaleIP
// docs). A relay destination inside this range is, by definition, another
// mesh node -- exactly the LAN/mesh pivot this policy exists to deny by
// default.
var cgnatBlock = func() *net.IPNet {
	_, n, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		panic(err) // unreachable: literal, valid CIDR
	}
	return n
}()

// IsDestinationAllowed reports whether a destination host is permitted by
// the relay's destination policy, and -- when it is not -- a human-readable
// reason.
//
// Deny-by-default (citadel #787's owner decision, "option 1"): loopback,
// unspecified (0.0.0.0 / ::), link-local, RFC1918 private ranges, and the
// 100.64.0.0/10 CGNAT/mesh range are refused unless allowLAN is true. Every
// other destination, including the public internet, is always allowed --
// this policy's job is narrowly to stop an authorized peer from using the
// relay to pivot into THIS node's own LAN or mesh, not to otherwise restrict
// where the relay can egress.
//
// host is expected to be a literal IP -- the resolved address a dialer is
// actually about to connect to, never an unresolved hostname. This function
// makes no network calls (no DNS resolution) and therefore CANNOT itself
// determine whether a hostname is safe; the caller (PolicyDialer, relay.go)
// is responsible for resolving a hostname FIRST and invoking this function
// once per resolved literal IP, then dialing exactly the address it
// validated. A non-IP string here is refused, not allowed -- see the "not a
// literal IP" case below for why, and its comment for the citadel #787
// incident this replaces (a hostname resolving to a private IP -- or a
// DNS-rebinding attacker answering differently on a second lookup -- used to
// sail through this function entirely unchecked).
func IsDestinationAllowed(host string, allowLAN bool) (bool, string) {
	if allowLAN {
		return true, ""
	}

	if strings.EqualFold(host, "localhost") {
		return false, "loopback destination denied (allow_lan is off)"
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// Not a literal IP. Refuse rather than guess: this function has no
		// way to know what a hostname resolves to, and a security-relevant
		// policy that defaults to "allow when unsure" is the exact shape of
		// bug that let a CONNECT to a hostname with a private-IP A-record
		// bypass the entire deny-list pre-fix. In production this branch
		// should essentially never be reached with a genuine hostname:
		// PolicyDialer resolves first and calls this function with the
		// resolved IP, never the raw name (localhost, handled above, is the
		// one deliberate exception -- a well-known loopback synonym checked
		// as text so denying it needs no resolution at all).
		return false, fmt.Sprintf("cannot verify unresolved destination %q without a literal IP (allow_lan is off)", host)
	}

	switch {
	case ip.IsUnspecified():
		// 0.0.0.0 / :: -- net.Dial resolves an unspecified address to
		// loopback on Linux (and elsewhere), so it is exactly as dangerous
		// as a literal 127.0.0.1/::1 CONNECT target here.
		return false, "unspecified destination denied (allow_lan is off)"
	case ip.IsLoopback():
		return false, "loopback destination denied (allow_lan is off)"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return false, "link-local destination denied (allow_lan is off)"
	case ip.IsPrivate():
		// Covers RFC1918 (10/8, 172.16/12, 192.168/16) and RFC4193
		// (fc00::/7) via the stdlib's own classification.
		return false, "private (RFC1918) destination denied (allow_lan is off)"
	case cgnatBlock.Contains(ip):
		return false, "CGNAT/mesh (100.64.0.0/10) destination denied (allow_lan is off)"
	default:
		return true, ""
	}
}
