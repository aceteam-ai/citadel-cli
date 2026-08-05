//go:build darwin

// internal/network/dns_darwin.go
// macOS MagicDNS fix: convert the global-resolver DNS config into a split-DNS
// one, because /etc/resolver can only express the latter (issue #676).
package network

import (
	"tailscale.com/net/dns"
	"tailscale.com/util/dnsname"
)

// splitDNSConfigurator wraps the macOS OSConfigurator so MagicDNS names
// actually resolve.
//
// The problem it solves. On macOS, `tailscaled`-style DNS is applied by
// writing files under /etc/resolver, and dns.darwinConfigurator.SetDNS emits
// the `nameserver` lines ONLY inside `for _, d := range cfg.MatchDomains`.
// But net/dns/manager.go deliberately excludes Apple from the native
// split-DNS path:
//
//	isApple := (m.goos == "darwin" || m.goos == "ios")
//	if m.os.SupportsSplitDNS() && !isWindows && !isApple { ... }
//
// and then only re-populates MatchDomains when the base config is missing, or
// on iOS. On macOS with a readable base config we therefore arrive with
// Nameservers set and MatchDomains EMPTY — "be the global resolver" — which
// /etc/resolver has no way to express. The loop never runs, only the
// search-domain file is written, and the nameservers are silently dropped.
//
// The symptom is nasty precisely because it is quiet: routing works, the
// MagicDNS server answers when queried directly (`dig @100.100.100.100
// peer.internal` is fine), the logs look healthy — but `ping peer.internal`
// cannot resolve, so a user concludes their DNS is broken rather than ours.
//
// The fix. When MatchDomains is empty but nameservers were requested, match
// the search domains the TAILNET contributed — that is, the ones absent from
// the OS's own base configuration. That converts the request into the split
// config /etc/resolver can represent.
//
// This is also the posture citadel wants independent of the macOS
// constraint: answer for the fabric domain, and leave every other query on
// the machine's existing resolver. Being a machine's global DNS is a much
// larger promise than machine-wide routing needs to make, and it makes a
// failed teardown far more consequential.
type splitDNSConfigurator struct {
	dns.OSConfigurator
}

// newSplitDNSConfigurator wraps base. A nil base is returned unchanged so
// callers can wrap unconditionally.
func newSplitDNSConfigurator(base dns.OSConfigurator) dns.OSConfigurator {
	if base == nil {
		return nil
	}
	return &splitDNSConfigurator{OSConfigurator: base}
}

func (c *splitDNSConfigurator) SetDNS(cfg dns.OSConfig) error {
	if len(cfg.MatchDomains) == 0 && len(cfg.Nameservers) > 0 {
		if matches := c.tailnetSearchDomains(cfg); len(matches) > 0 {
			logf("dns: macOS global-resolver config has no MatchDomains; "+
				"scoping to tailnet domains %v so /etc/resolver can express it (#676)", matches)
			cfg.MatchDomains = matches
		} else {
			// Leave cfg untouched rather than guessing. Behaviour is then
			// exactly what it was before this wrapper existed — names will
			// not resolve, but nothing else regresses.
			logf("dns: macOS global-resolver config has no MatchDomains and no " +
				"tailnet-specific search domains; leaving as-is (MagicDNS names will not resolve)")
		}
	}
	return c.OSConfigurator.SetDNS(cfg)
}

// tailnetSearchDomains returns the search domains that came from the tailnet
// rather than from the machine's existing network configuration.
//
// Subtracting the base config is what keeps this honest: the OS search list
// on a typical LAN already carries something like "home.", and matching that
// would route ordinary local lookups through the mesh resolver for no reason.
// Only the domains the tailnet added — e.g. "internal." — belong to us.
//
// If the base config cannot be read, this returns nothing and the caller
// leaves the config alone. Guessing here would risk capturing the machine's
// LAN domain.
func (c *splitDNSConfigurator) tailnetSearchDomains(cfg dns.OSConfig) []dnsname.FQDN {
	if len(cfg.SearchDomains) == 0 {
		return nil
	}

	base, err := c.OSConfigurator.GetBaseConfig()
	if err != nil {
		logf("dns: cannot read base config to identify tailnet domains: %v", err)
		return nil
	}

	inBase := make(map[dnsname.FQDN]bool, len(base.SearchDomains))
	for _, d := range base.SearchDomains {
		inBase[d] = true
	}

	var out []dnsname.FQDN
	for _, d := range cfg.SearchDomains {
		if !inBase[d] {
			out = append(out, d)
		}
	}
	return out
}
