package jobs

import (
	"fmt"
	"net"
	"net/netip"
)

// interfaceAddrsFn lists this host's interface addresses. A package variable
// so tests can supply a fixed set, or an error, instead of the machine's real
// interfaces.
var interfaceAddrsFn = net.InterfaceAddrs

// ownAddrSet is the set of addresses assigned to this host's own interfaces
// (LAN, docker bridges, and in TUN or attached mode the AceTeam Network
// address). A node-side relay that dials one of these reaches the node's own
// services, which may bind every interface, so relays treat them like loopback
// (citadel-cli#1318, #1326).
type ownAddrSet map[netip.Addr]bool

// has reports whether ip is one of the node's own addresses. Ownership ignores
// IPv6 zones and IPv4-mapped forms: fd7a::42%eth0 is the same host address as
// fd7a::42, so a zoned destination can't slip past the check.
func (s ownAddrSet) has(ip netip.Addr) bool {
	return s[normalizeOwnAddr(ip)]
}

func normalizeOwnAddr(ip netip.Addr) netip.Addr {
	return ip.Unmap().WithZone("")
}

// ownInterfaceAddrs returns the node's own interface addresses. An enumeration
// error is returned, never turned into an empty set: callers must refuse when
// they cannot tell the node's own addresses from a peer's (fail closed).
func ownInterfaceAddrs() (ownAddrSet, error) {
	addrs, err := interfaceAddrsFn()
	if err != nil {
		return nil, fmt.Errorf("cannot determine this node's own addresses: %w", err)
	}
	own := make(ownAddrSet, len(addrs))
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		if addr, ok := netip.AddrFromSlice(ip); ok {
			own[normalizeOwnAddr(addr)] = true
		}
	}
	return own, nil
}
