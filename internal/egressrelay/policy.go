package egressrelay

import (
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

// IsDestinationAllowed reports whether a SOCKS5 CONNECT target host is
// permitted by the relay's destination policy, and -- when it is not -- a
// human-readable reason.
//
// Deny-by-default (citadel #787's owner decision, "option 1"): loopback,
// link-local, RFC1918 private ranges, and the 100.64.0.0/10 CGNAT/mesh range
// are refused unless allowLAN is true. Every other destination, including
// the public internet, is always allowed -- this policy's job is narrowly to
// stop an authorized peer from using the relay to pivot into THIS node's own
// LAN or mesh, not to otherwise restrict where the relay can egress.
//
// host may be a literal IP or a hostname. A hostname that is not a literal
// IP (and is not the "localhost" name, checked explicitly since it never
// appears as a literal IP on the wire) is always allowed by this function:
// DNS resolution happens inside the underlying dialer, exactly like
// internal/socks itself never pre-resolves a DOMAINNAME target before
// deciding whether to dial it. A hostname that RESOLVES to a private/CGNAT
// address is therefore a documented residual gap, not a bypass this
// function can close without doing its own DNS resolution before every
// dial -- see the PR description for citadel #787.
func IsDestinationAllowed(host string, allowLAN bool) (bool, string) {
	if allowLAN {
		return true, ""
	}

	if strings.EqualFold(host, "localhost") {
		return false, "loopback destination denied (allow_lan is off)"
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// Not a literal IP: allow. See doc comment above.
		return true, ""
	}

	switch {
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
