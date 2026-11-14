package localcobrowse

import (
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// UnknownDomain is the grant key for a page whose address has no host. A write
// there still needs its own approval; nothing is ever granted by default.
const UnknownDomain = "(unknown page)"

// RegistrableDomain returns the grant scope for a URL (aceteam#13687 D3): the
// registrable domain from the public suffix list (so shop.example.com and
// www.example.com share a grant, while a.github.io and b.github.io do not).
// An IP address or a single-label host is its own scope. about:blank is its own
// scope. Anything without a host returns UnknownDomain, never "".
func RegistrableDomain(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "about:blank") {
		return "about:blank"
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return UnknownDomain
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	if ascii, err := idna.Lookup.ToASCII(host); err == nil {
		host = ascii
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return host
	}
	return d
}
