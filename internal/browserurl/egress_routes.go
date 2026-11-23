package browserurl

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"strconv"
	"strings"
)

// OnLinkRoutes lists the directly connected routes of this machine: routes
// with no gateway, whose destinations are reached on the link itself. An
// interface address reported as /128 (stateful DHCPv6) or /32 says nothing
// about the LAN around it; the route table does.
type OnLinkRoutes func() ([]netip.Prefix, error)

// onLinkRoutes is the per-OS route reader (egress_routes_<os>.go). A package
// var so tests substitute fake tables.
var onLinkRoutes OnLinkRoutes = osOnLinkRoutes

// DefaultInterfacePrefixes is this machine's interface addresses with their
// masks plus its directly connected routes. When the routes cannot be read the
// interface masks alone are used, as before.
func DefaultInterfacePrefixes() ([]netip.Prefix, error) {
	return combineOwnNet(interfaceAddrPrefixes, onLinkRoutes)
}

func combineOwnNet(ifaces InterfacePrefixes, routes OnLinkRoutes) ([]netip.Prefix, error) {
	out, err := ifaces()
	if err != nil {
		return nil, err
	}
	out = append(out, ipv6Slash64s(out)...)
	if routes != nil {
		if r, rerr := routes(); rerr == nil {
			out = append(out, r...)
		}
	}
	return out, nil
}

// ipv6Slash64s returns the /64 around each global IPv6 interface address
// reported with a longer prefix. A stateful DHCPv6 address is often /128 and,
// when the router advertises its prefix as off-link (L=0), no connected route
// names the LAN either. A /64 is the standard IPv6 subnet, so it is treated as
// on-link (the usual ownNet floor still applies).
func ipv6Slash64s(list []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range list {
		a := p.Addr()
		if !a.Is6() || a.Is4In6() || !a.IsGlobalUnicast() || p.Bits() <= 64 {
			continue
		}
		out = append(out, netip.PrefixFrom(a.WithZone(""), 64).Masked())
	}
	return out
}

// Linux route flags (include/uapi/linux/route.h, ipv6_route.h).
const (
	linuxRTFUp      = 0x0001
	linuxRTFGateway = 0x0002
	linuxRTFReject  = 0x0200
)

// parseLinuxIPv4Routes reads /proc/net/route. Addresses there are the raw
// 32-bit network-order values printed as host integers, so order is the host
// byte order (binary.NativeEndian in production).
func parseLinuxIPv4Routes(table string, order binary.ByteOrder) []netip.Prefix {
	var out []netip.Prefix
	sc := bufio.NewScanner(strings.NewReader(table))
	first := true
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if first {
			first = false
			if len(f) > 0 && f[0] == "Iface" {
				continue
			}
		}
		if len(f) < 8 {
			continue
		}
		dst, ok1 := hexIPv4(f[1], order)
		gw, ok2 := hexIPv4(f[2], order)
		flags, err := strconv.ParseUint(f[3], 16, 32)
		mask, ok3 := hexIPv4(f[7], order)
		if !ok1 || !ok2 || !ok3 || err != nil {
			continue
		}
		if flags&linuxRTFUp == 0 || flags&(linuxRTFGateway|linuxRTFReject) != 0 || !gw.IsUnspecified() {
			continue
		}
		ones, ok := maskBits(mask.AsSlice())
		if !ok {
			continue
		}
		out = append(out, netip.PrefixFrom(dst, ones).Masked())
	}
	return out
}

func hexIPv4(s string, order binary.ByteOrder) (netip.Addr, bool) {
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return netip.Addr{}, false
	}
	var b [4]byte
	order.PutUint32(b[:], uint32(v))
	return netip.AddrFrom4(b), true
}

// parseLinuxIPv6Routes reads /proc/net/ipv6_route: destination, prefix
// length, source, source length, next hop, metric, refcnt, use, flags, iface.
func parseLinuxIPv6Routes(table string) []netip.Prefix {
	var out []netip.Prefix
	sc := bufio.NewScanner(strings.NewReader(table))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		dst, ok1 := hexIPv6(f[0])
		plen, err1 := strconv.ParseUint(f[1], 16, 8)
		nh, ok2 := hexIPv6(f[4])
		flags, err2 := strconv.ParseUint(f[8], 16, 32)
		if !ok1 || !ok2 || err1 != nil || err2 != nil || plen > 128 {
			continue
		}
		if flags&linuxRTFUp == 0 || flags&(linuxRTFGateway|linuxRTFReject) != 0 || !nh.IsUnspecified() {
			continue
		}
		out = append(out, netip.PrefixFrom(dst, int(plen)).Masked())
	}
	return out
}

func hexIPv6(s string) (netip.Addr, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		return netip.Addr{}, false
	}
	return netip.AddrFrom16([16]byte(b)), true
}

// maskBits counts a contiguous netmask; a non-contiguous one is refused.
func maskBits(m []byte) (int, bool) {
	ones := 0
	seenZero := false
	for _, b := range m {
		for i := 7; i >= 0; i-- {
			if b&(1<<i) != 0 {
				if seenZero {
					return 0, false
				}
				ones++
			} else {
				seenZero = true
			}
		}
	}
	return ones, true
}

// Windows address families (ws2def.h).
const (
	winAFInet  = 2
	winAFInet6 = 23
)

// windowsSockaddrAddr decodes a SOCKADDR_INET (family, then the sockaddr_in or
// sockaddr_in6 body) from its raw bytes.
func windowsSockaddrAddr(raw [28]byte) (netip.Addr, bool) {
	switch binary.LittleEndian.Uint16(raw[0:2]) {
	case winAFInet:
		return netip.AddrFrom4([4]byte(raw[4:8])), true
	case winAFInet6:
		return netip.AddrFrom16([16]byte(raw[8:24])), true
	}
	return netip.Addr{}, false
}

// windowsOnLinkRoute turns one MIB_IPFORWARD_ROW2 into an on-link prefix: the
// next hop is unspecified for a directly connected route.
func windowsOnLinkRoute(dst [28]byte, prefixLen uint8, nextHop [28]byte) (netip.Prefix, bool) {
	a, ok := windowsSockaddrAddr(dst)
	if !ok {
		return netip.Prefix{}, false
	}
	nh, ok := windowsSockaddrAddr(nextHop)
	if !ok || !nh.IsUnspecified() || int(prefixLen) > a.BitLen() {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, int(prefixLen)).Masked(), true
}

// darwinOnLinkRoute turns one routing message into an on-link prefix. mask is
// nil for a host route (a neighbor entry). gateway reports RTF_GATEWAY.
func darwinOnLinkRoute(dst netip.Addr, mask []byte, gateway, reject bool) (netip.Prefix, bool) {
	if gateway || reject || !dst.IsValid() {
		return netip.Prefix{}, false
	}
	n := dst.BitLen()
	if mask != nil {
		// BSD masks may be shortened to their significant bytes.
		full := make([]byte, n/8)
		copy(full, mask)
		ones, ok := maskBits(full)
		if !ok {
			return netip.Prefix{}, false
		}
		n = ones
	}
	return netip.PrefixFrom(dst, n).Masked(), true
}
