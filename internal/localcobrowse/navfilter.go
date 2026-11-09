package localcobrowse

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// ErrNavigationRefused is returned for a URL the local co-browse browser must
// not open: a non-web scheme, or an address on this machine or a private
// network. It keeps a prompt-injected page from steering the agent at the
// local viewer, the browser's own debug port, or other local services.
var ErrNavigationRefused = errors.New("navigation refused")

// Resolver resolves a host name to its addresses.
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

// DefaultResolver uses the system resolver.
func DefaultResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// blockedPrefixes are refused in addition to the netip predicates checked in
// blockedAddr (loopback, private, link-local, multicast, unspecified).
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT, also mesh addresses
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, incl. broadcast
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("fec0::/10"),      // deprecated site-local
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("2002::/16"),      // 6to4 (embeds an IPv4 address)
	netip.MustParsePrefix("2001::/32"),      // Teredo (embeds an IPv4 address)
	netip.MustParsePrefix("::ffff:0:0/96"),  // IPv4-mapped (checked after unmapping too)
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix("ff00::/8"),       // multicast
	netip.MustParsePrefix("fc00::/7"),       // unique local
	netip.MustParsePrefix("fe80::/10"),      // link-local
	netip.MustParsePrefix("::/128"),         // unspecified
	netip.MustParsePrefix("::1/128"),        // loopback
	netip.MustParsePrefix("169.254.0.0/16"), // link-local
	netip.MustParsePrefix("127.0.0.0/8"),    // loopback
	netip.MustParsePrefix("10.0.0.0/8"),     // private
	netip.MustParsePrefix("172.16.0.0/12"),  // private
	netip.MustParsePrefix("192.168.0.0/16"), // private
	netip.MustParsePrefix("224.0.0.0/4"),    // multicast
	netip.MustParsePrefix("255.255.255.255/32"),
}

var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// blockedAddr reports whether a is not a public internet address.
func blockedAddr(a netip.Addr) bool {
	if !a.IsValid() {
		return true
	}
	if a.Is4In6() {
		if blockedAddr(a.Unmap()) {
			return true
		}
	}
	if nat64.Contains(a) {
		b := a.As16()
		v4 := netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
		if blockedAddr(v4) {
			return true
		}
	}
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// URLPolicy decides whether the co-browse browser may open a URL.
type URLPolicy func(ctx context.Context, raw string) error

// NewURLPolicy returns the production policy: http and https only, the host must
// resolve only to public addresses, and blockedPorts (the local viewer's) are
// refused outright. about:blank is allowed so a session can start empty.
func NewURLPolicy(resolve Resolver, blockedPorts func() []int) URLPolicy {
	return func(ctx context.Context, raw string) error {
		return checkURL(ctx, raw, resolve, blockedPorts)
	}
}

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrNavigationRefused, fmt.Sprintf(format, a...))
}

func checkURL(ctx context.Context, raw string, resolve Resolver, blockedPorts func() []int) error {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, "about:blank") {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return refuse("not a valid URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return refuse("only http and https pages can be opened (got %q)", u.Scheme)
	}
	if u.User != nil {
		return refuse("URLs with embedded credentials are not allowed")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return refuse("URL has no host")
	}
	if port := u.Port(); port != "" && blockedPorts != nil {
		if p, err := strconv.Atoi(port); err == nil {
			for _, b := range blockedPorts() {
				if b == p {
					return refuse("that port belongs to the local Citadel viewer")
				}
			}
		}
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return refuse("addresses on this computer are not allowed")
	}
	if addr, ok := parseBrowserHost(host); ok {
		if blockedAddr(addr) {
			return refuse("private, local and reserved addresses are not allowed (%s)", addr)
		}
		return nil
	}
	if looksNumericHost(host) {
		// A browser would either treat this as an IPv4 address in a form we did
		// not parse, or refuse it. Either way, do not open it.
		return refuse("unrecognized numeric host %q", host)
	}
	if resolve == nil {
		return refuse("no resolver configured")
	}
	addrs, err := resolve(ctx, host)
	if err != nil {
		return refuse("could not resolve %s", host)
	}
	if len(addrs) == 0 {
		return refuse("%s did not resolve", host)
	}
	for _, a := range addrs {
		if blockedAddr(a) {
			return refuse("%s resolves to a private or local address (%s)", host, a)
		}
	}
	return nil
}

// parseBrowserHost parses host the way a browser's URL parser treats an IPv4
// or IPv6 host: bracketed IPv6, and IPv4 in one to four dot-separated parts,
// each decimal, octal (leading 0) or hex (0x). So 2130706433, 0x7f.1 and
// 0177.0.0.1 all parse as 127.0.0.1.
func parseBrowserHost(host string) (netip.Addr, bool) {
	if strings.Contains(host, ":") {
		a, err := netip.ParseAddr(strings.Trim(host, "[]"))
		if err != nil {
			return netip.Addr{}, false
		}
		return a.WithZone(""), true
	}
	parts := strings.Split(host, ".")
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 || len(parts) > 4 {
		return netip.Addr{}, false
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		n, ok := parseIPv4Part(p)
		if !ok {
			return netip.Addr{}, false
		}
		nums[i] = n
	}
	for _, n := range nums[:len(nums)-1] {
		if n > 255 {
			return netip.Addr{}, false
		}
	}
	last := nums[len(nums)-1]
	if last >= 1<<(8*(5-len(nums))) {
		return netip.Addr{}, false
	}
	v := last
	for i, n := range nums[:len(nums)-1] {
		v |= n << (8 * (3 - i))
	}
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}

func parseIPv4Part(p string) (uint64, bool) {
	if p == "" {
		return 0, false
	}
	base := 10
	switch {
	case strings.HasPrefix(p, "0x") || strings.HasPrefix(p, "0X"):
		base, p = 16, p[2:]
		if p == "" {
			return 0, true
		}
	case len(p) > 1 && p[0] == '0':
		base, p = 8, p[1:]
	}
	n, err := strconv.ParseUint(p, base, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// looksNumericHost reports a host whose last label is numeric, which a browser
// treats as an IPv4 address attempt rather than a domain.
func looksNumericHost(host string) bool {
	parts := strings.Split(strings.TrimSuffix(host, "."), ".")
	last := parts[len(parts)-1]
	if last == "" {
		return false
	}
	if strings.HasPrefix(last, "0x") || strings.HasPrefix(last, "0X") {
		return true
	}
	for _, c := range last {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
