package jobs

import (
	"net"
	"net/netip"
)

// interfaceAddrsFn lists this host's interface addresses. A package variable
// so tests can supply a fixed set instead of the machine's real interfaces.
var interfaceAddrsFn = net.InterfaceAddrs

// ownInterfaceAddrs returns every address assigned to one of this host's own
// interfaces (LAN, docker bridges, and in TUN or attached mode the AceTeam
// Network address). A node-side relay that dials one of these reaches the
// node's own services, which may bind every interface, so relays treat them
// like loopback (citadel-cli#1318, #1326). An enumeration error yields an
// empty set; callers still refuse loopback and link-local on their own.
func ownInterfaceAddrs() map[netip.Addr]bool {
	addrs, err := interfaceAddrsFn()
	if err != nil {
		return map[netip.Addr]bool{}
	}
	own := make(map[netip.Addr]bool, len(addrs))
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
			own[addr.Unmap()] = true
		}
	}
	return own
}
