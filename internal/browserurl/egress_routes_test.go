package browserurl

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"reflect"
	"testing"
)

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// A /proc/net/route table in little-endian host order: the default route via
// a gateway, a directly connected public LAN, a rejected route and a route
// that is down. Documentation addresses only.
const linuxRouteTable = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0164A8C0	0003	0	0	100	00000000	0	0	0
eth0	006433C6	00000000	0001	0	0	100	00FFFFFF	0	0	0
eth0	000071CB	00000000	0201	0	0	0	00FFFFFF	0	0	0
eth1	000201C0	00000000	0000	0	0	0	00FFFFFF	0	0	0
eth0	1400A8C0	1400A8C0	0007	0	0	0	FFFFFFFF	0	0	0
`

func TestParseLinuxIPv4Routes(t *testing.T) {
	got := parseLinuxIPv4Routes(linuxRouteTable, binary.LittleEndian)
	if want := prefixes("198.51.100.0/24"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// The same table printed by a big-endian host.
	be := "Iface Destination Gateway Flags RefCnt Use Metric Mask\neth0 C6336400 00000000 0001 0 0 100 FFFFFF00\n"
	if got := parseLinuxIPv4Routes(be, binary.BigEndian); !reflect.DeepEqual(got, prefixes("198.51.100.0/24")) {
		t.Fatalf("big-endian: %v", got)
	}
	// A non-contiguous mask is ignored.
	odd := "Iface Destination Gateway Flags RefCnt Use Metric Mask\neth0 006433C6 00000000 0001 0 0 100 00FF00FF\n"
	if got := parseLinuxIPv4Routes(odd, binary.LittleEndian); len(got) != 0 {
		t.Fatalf("non-contiguous mask: %v", got)
	}
}

// /proc/net/ipv6_route: a connected global /64, the default route via a
// router, the kernel's unreachable route and a connected route that is down.
const linuxIPv6RouteTable = `20010db8000a00010000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth0
00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003     eth0
00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo
20010db8000b00010000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000000     eth1
20010db8000a00010000000000000005 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000002 00000000 80200001       lo
`

func TestParseLinuxIPv6Routes(t *testing.T) {
	got := parseLinuxIPv6Routes(linuxIPv6RouteTable)
	want := prefixes("2001:db8:a:1::/64", "2001:db8:a:1::5/128")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func rawSockaddr4(a string) [28]byte {
	var r [28]byte
	binary.LittleEndian.PutUint16(r[0:2], winAFInet)
	b := netip.MustParseAddr(a).As4()
	copy(r[4:8], b[:])
	return r
}

func rawSockaddr6(a string) [28]byte {
	var r [28]byte
	binary.LittleEndian.PutUint16(r[0:2], winAFInet6)
	b := netip.MustParseAddr(a).As16()
	copy(r[8:24], b[:])
	return r
}

func TestWindowsOnLinkRoute(t *testing.T) {
	cases := []struct {
		dst  [28]byte
		plen uint8
		nh   [28]byte
		want string
	}{
		{rawSockaddr4("198.51.100.0"), 24, rawSockaddr4("0.0.0.0"), "198.51.100.0/24"},
		{rawSockaddr4("0.0.0.0"), 0, rawSockaddr4("192.168.100.1"), ""}, // via a gateway
		{rawSockaddr6("2001:db8:a:1::"), 64, rawSockaddr6("::"), "2001:db8:a:1::/64"},
		{rawSockaddr6("::"), 0, rawSockaddr6("fe80::1"), ""},
		{rawSockaddr4("198.51.100.0"), 40, rawSockaddr4("0.0.0.0"), ""}, // bad length
		{[28]byte{}, 0, [28]byte{}, ""},                                 // unknown family
	}
	for i, tc := range cases {
		p, ok := windowsOnLinkRoute(tc.dst, tc.plen, tc.nh)
		if tc.want == "" {
			if ok {
				t.Errorf("case %d: unexpected route %v", i, p)
			}
			continue
		}
		if !ok || p != netip.MustParsePrefix(tc.want) {
			t.Errorf("case %d: got %v %v, want %s", i, p, ok, tc.want)
		}
	}
}

func TestDarwinOnLinkRoute(t *testing.T) {
	cases := []struct {
		dst     string
		mask    []byte
		gw, rej bool
		want    string
	}{
		{"198.51.100.0", []byte{255, 255, 255}, false, false, "198.51.100.0/24"}, // shortened mask
		{"198.51.100.20", nil, false, false, "198.51.100.20/32"},                 // neighbor entry
		{"0.0.0.0", []byte{}, true, false, ""},                                   // default via gateway
		{"2001:db8:a:1::", []byte{255, 255, 255, 255, 255, 255, 255, 255}, false, false, "2001:db8:a:1::/64"},
		{"203.0.113.0", []byte{255, 255, 255}, false, true, ""}, // reject
		{"198.51.100.0", []byte{255, 0, 255}, false, false, ""}, // non-contiguous
	}
	for _, tc := range cases {
		p, ok := darwinOnLinkRoute(netip.MustParseAddr(tc.dst), tc.mask, tc.gw, tc.rej)
		if tc.want == "" {
			if ok {
				t.Errorf("%s: unexpected route %v", tc.dst, p)
			}
			continue
		}
		if !ok || p != netip.MustParsePrefix(tc.want) {
			t.Errorf("%s: got %v %v, want %s", tc.dst, p, ok, tc.want)
		}
	}
}

// TestOnLinkRoutesCoverHostMaskAddress is the stateful DHCPv6 case: the
// interface reports its address as /128 (and an IPv4 one as /32), so only the
// connected route says the router and neighbors are on the link.
func TestOnLinkRoutesCoverHostMaskAddress(t *testing.T) {
	ifaces := fakeIfaces("2001:db8:a:1::5/128", "198.51.100.7/32")
	routes := func() ([]netip.Prefix, error) { return prefixes("2001:db8:a:1::/64", "198.51.100.0/24"), nil }

	withRoutes := newOwnNet(func() ([]netip.Prefix, error) { return combineOwnNet(ifaces, routes) })
	masksOnly := newOwnNet(ifaces)
	for _, a := range []string{"2001:db8:a:1::1", "198.51.100.1"} {
		if !withRoutes.contains(netip.MustParseAddr(a)) {
			t.Errorf("%s: the on-link router must be refused", a)
		}
		if masksOnly.contains(netip.MustParseAddr(a)) {
			t.Errorf("%s: control: interface masks alone do not cover it", a)
		}
	}
	if withRoutes.contains(netip.MustParseAddr("2001:db8:b::1")) {
		t.Error("an off-link address was refused")
	}

	// A failed route read falls back to the interface masks.
	broken := func() ([]netip.Prefix, error) { return nil, errors.New("no route table") }
	got, err := combineOwnNet(ifaces, broken)
	if err != nil || !reflect.DeepEqual(got, prefixes("2001:db8:a:1::5/128", "198.51.100.7/32")) {
		t.Fatalf("fallback: %v %v", got, err)
	}
	// An interface read failure is still an error (the guard then fails closed).
	if _, err := combineOwnNet(func() ([]netip.Prefix, error) { return nil, errors.New("x") }, routes); err == nil {
		t.Fatal("interface failure must surface")
	}
}

// TestOnLinkRouteFloorStillApplies: a connected route wider than the floor
// (a VPN device route) blocks only its own address, as for interface masks.
func TestOnLinkRouteFloorStillApplies(t *testing.T) {
	o := newOwnNet(func() ([]netip.Prefix, error) { return prefixes("0.0.0.0/1", "2000::/3"), nil })
	for _, a := range []string{"8.8.8.8", "2001:db8:c::1"} {
		if o.contains(netip.MustParseAddr(a)) {
			t.Errorf("%s refused by a route wider than the floor", a)
		}
	}
}

func TestOSOnLinkRoutesReadable(t *testing.T) {
	r, err := osOnLinkRoutes()
	if err != nil {
		t.Skipf("route table not readable here: %v", err)
	}
	t.Logf("%d connected route(s)", len(r))
}
