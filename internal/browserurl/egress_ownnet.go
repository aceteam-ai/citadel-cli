package browserurl

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// InterfacePrefixes lists this machine's interface addresses, each with its
// on-link prefix length (as net.InterfaceAddrs reports them).
type InterfacePrefixes func() ([]netip.Prefix, error)

// DefaultInterfacePrefixes reads the machine's interface addresses.
func DefaultInterfacePrefixes() ([]netip.Prefix, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Prefix, 0, len(addrs))
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		ones, size := n.Mask.Size()
		ip = ip.Unmap()
		if ip.Is4() && size == 128 {
			ones -= 96
		}
		if p := netip.PrefixFrom(ip, ones); p.IsValid() {
			out = append(out, p)
		}
	}
	return out, nil
}

// ownNetTTL bounds how stale the interface list may be: SLAAC temporary
// IPv6 addresses rotate, and a laptop changes networks.
const ownNetTTL = 30 * time.Second

// Shortest on-link prefixes treated as this machine's own network. A shorter
// one (a VPN route covering a large share of the internet) still blocks the
// interface address itself, but not the whole prefix.
const (
	minOwnNetBits4 = 8
	minOwnNetBits6 = 32
)

// ownNet answers whether an address is this machine's own or on one of its
// directly attached networks. A machine with a public IPv4 LAN or a global
// IPv6 /64 has neighbors (the router at prefix::1, a NAS, its own services
// listening on ::) that BlockedAddr alone reads as public.
type ownNet struct {
	list    InterfacePrefixes
	mu      sync.Mutex
	at      time.Time
	cached  []netip.Prefix
	fetched bool
	now     func() time.Time
}

func newOwnNet(list InterfacePrefixes) *ownNet {
	if list == nil {
		list = DefaultInterfacePrefixes
	}
	return &ownNet{list: list, now: time.Now}
}

// prefixes returns the cached list, refreshing it after ownNetTTL. When a
// refresh fails the previous list is kept; with no list at all, ok is false
// and the caller refuses (it cannot tell what is local).
func (o *ownNet) prefixes() ([]netip.Prefix, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.fetched || o.now().Sub(o.at) >= ownNetTTL {
		if l, err := o.list(); err == nil {
			o.cached, o.at, o.fetched = l, o.now(), true
		} else if !o.fetched {
			return nil, false
		}
	}
	return o.cached, true
}

// contains reports whether a equals an interface address or falls inside an
// interface's on-link prefix. It fails closed when the list is unavailable.
func (o *ownNet) contains(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	list, ok := o.prefixes()
	if !ok {
		return true
	}
	for _, p := range list {
		ip := p.Addr().Unmap().WithZone("")
		if ip == a {
			return true
		}
		if ip.Is4() != a.Is4() {
			continue
		}
		min := minOwnNetBits6
		if ip.Is4() {
			min = minOwnNetBits4
		}
		if p.Bits() < min {
			continue
		}
		if netip.PrefixFrom(ip, p.Bits()).Masked().Contains(a) {
			return true
		}
	}
	return false
}
