package localcobrowse

import (
	"net/url"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// UnknownDomain is the grant key for a page whose address cannot be scoped: no
// host, a non-web scheme, or a numeric host a browser might read as an address.
// It can never be approved (ApproveDomain refuses it) and a write there is
// refused outright, so nothing is granted for a page we cannot name.
const UnknownDomain = "(unknown page)"

// RegistrableDomain returns the grant scope for a URL (aceteam#13687 D3): the
// registrable domain from the public suffix list (so shop.example.com and
// www.example.com share a grant, while a.github.io and b.github.io do not).
// It reads the host the way a browser and URLPolicy do: userinfo and port are
// ignored, a trailing dot and case are dropped, IDN is converted to ASCII, and
// IPv4 in decimal, octal or hex forms and IPv6 become their canonical address
// (an IP or a single-label host is its own scope). about:blank is its own
// scope. Anything else returns UnknownDomain, never "".
func RegistrableDomain(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, "about:blank") {
		return "about:blank"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return UnknownDomain
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return UnknownDomain
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return UnknownDomain
	}
	if addr, ok := parseBrowserHost(host); ok {
		return addr.Unmap().String()
	}
	if looksNumericHost(host) {
		return UnknownDomain
	}
	if ascii, err := idna.Lookup.ToASCII(host); err == nil {
		host = ascii
	} else {
		return UnknownDomain
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return host
	}
	return d
}
