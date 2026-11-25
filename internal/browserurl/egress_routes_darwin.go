//go:build darwin

package browserurl

import (
	"net/netip"
	"syscall"

	"golang.org/x/net/route"
)

// osOnLinkRoutes reads the routing table through the route sysctl and keeps
// routes without RTF_GATEWAY (directly connected networks and neighbors).
func osOnLinkRoutes() ([]netip.Prefix, error) {
	rib, err := route.FetchRIB(syscall.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return nil, err
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		if !ok || rm.Flags&syscall.RTF_UP == 0 || len(rm.Addrs) <= syscall.RTAX_NETMASK {
			continue
		}
		dst, ok := darwinAddr(rm.Addrs[syscall.RTAX_DST])
		if !ok {
			continue
		}
		var mask []byte
		if rm.Flags&syscall.RTF_HOST == 0 {
			switch a := rm.Addrs[syscall.RTAX_NETMASK].(type) {
			case *route.Inet4Addr:
				mask = a.IP[:]
			case *route.Inet6Addr:
				mask = a.IP[:]
			default:
				// A network route with no mask is the default route.
				mask = []byte{}
			}
		}
		gw := rm.Flags&syscall.RTF_GATEWAY != 0
		rej := rm.Flags&(syscall.RTF_REJECT|syscall.RTF_BLACKHOLE) != 0
		if p, ok := darwinOnLinkRoute(dst, mask, gw, rej); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func darwinAddr(a route.Addr) (netip.Addr, bool) {
	switch v := a.(type) {
	case *route.Inet4Addr:
		return netip.AddrFrom4(v.IP), true
	case *route.Inet6Addr:
		return netip.AddrFrom16(v.IP), true
	}
	return netip.Addr{}, false
}
