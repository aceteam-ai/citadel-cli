//go:build darwin

// internal/network/dns_darwin_test.go
package network

import (
	"errors"
	"testing"

	"net/netip"

	"tailscale.com/net/dns"
	"tailscale.com/util/dnsname"
)

// fakeOSConfigurator records what SetDNS was ultimately handed, so tests can
// assert on the config that would reach /etc/resolver.
type fakeOSConfigurator struct {
	got dns.OSConfig

	base    dns.OSConfig
	baseErr error

	splitDNS bool
	setErr   error
}

func (f *fakeOSConfigurator) SetDNS(cfg dns.OSConfig) error {
	f.got = cfg
	return f.setErr
}
func (f *fakeOSConfigurator) SupportsSplitDNS() bool               { return f.splitDNS }
func (f *fakeOSConfigurator) GetBaseConfig() (dns.OSConfig, error) { return f.base, f.baseErr }
func (f *fakeOSConfigurator) Close() error                         { return nil }

func fqdns(t *testing.T, names ...string) []dnsname.FQDN {
	t.Helper()
	out := make([]dnsname.FQDN, 0, len(names))
	for _, n := range names {
		f, err := dnsname.ToFQDN(n)
		if err != nil {
			t.Fatalf("ToFQDN(%q): %v", n, err)
		}
		out = append(out, f)
	}
	return out
}

func quad100(t *testing.T) []netip.Addr {
	t.Helper()
	return []netip.Addr{netip.MustParseAddr("100.100.100.100")}
}

// The bug from #676: the DNS manager hands macOS a global-resolver config
// (nameservers set, MatchDomains empty). darwinConfigurator writes the
// `nameserver` lines only for MatchDomains, so with none it writes no
// resolver at all and MagicDNS names silently fail to resolve.
func TestSplitDNSScopesGlobalConfigToTailnetDomains(t *testing.T) {
	fake := &fakeOSConfigurator{
		// The machine's own search list, as it exists without citadel.
		base: dns.OSConfig{SearchDomains: fqdns(t, "home")},
	}
	c := newSplitDNSConfigurator(fake)

	err := c.SetDNS(dns.OSConfig{
		Nameservers:   quad100(t),
		SearchDomains: fqdns(t, "internal", "home"),
	})
	if err != nil {
		t.Fatalf("SetDNS() error = %v", err)
	}

	if len(fake.got.MatchDomains) != 1 {
		t.Fatalf("MatchDomains = %v, want exactly [internal.]", fake.got.MatchDomains)
	}
	if got := fake.got.MatchDomains[0].WithoutTrailingDot(); got != "internal" {
		t.Errorf("MatchDomains[0] = %q, want internal", got)
	}
	// "home." came from the OS, not the tailnet — capturing it would route
	// ordinary LAN lookups through the mesh resolver.
	for _, d := range fake.got.MatchDomains {
		if d.WithoutTrailingDot() == "home" {
			t.Error("matched the machine's own LAN search domain; must only match tailnet domains")
		}
	}
	// Nameservers must survive untouched — they are the whole point.
	if len(fake.got.Nameservers) != 1 || fake.got.Nameservers[0].String() != "100.100.100.100" {
		t.Errorf("Nameservers = %v, want [100.100.100.100]", fake.got.Nameservers)
	}
}

// A config that already carries MatchDomains is a genuine split config; the
// wrapper must not touch it.
func TestSplitDNSLeavesExistingMatchDomainsAlone(t *testing.T) {
	fake := &fakeOSConfigurator{base: dns.OSConfig{SearchDomains: fqdns(t, "home")}}
	c := newSplitDNSConfigurator(fake)

	want := fqdns(t, "already.set")
	if err := c.SetDNS(dns.OSConfig{
		Nameservers:   quad100(t),
		MatchDomains:  want,
		SearchDomains: fqdns(t, "internal", "home"),
	}); err != nil {
		t.Fatalf("SetDNS() error = %v", err)
	}

	if len(fake.got.MatchDomains) != 1 || fake.got.MatchDomains[0] != want[0] {
		t.Errorf("MatchDomains = %v, want %v unchanged", fake.got.MatchDomains, want)
	}
}

// The zero config is how the manager tears DNS down. It must pass through
// untouched or teardown would leave resolver files behind.
func TestSplitDNSPassesThroughTeardown(t *testing.T) {
	fake := &fakeOSConfigurator{base: dns.OSConfig{SearchDomains: fqdns(t, "home")}}
	c := newSplitDNSConfigurator(fake)

	if err := c.SetDNS(dns.OSConfig{}); err != nil {
		t.Fatalf("SetDNS() error = %v", err)
	}
	if len(fake.got.MatchDomains) != 0 || len(fake.got.Nameservers) != 0 {
		t.Errorf("teardown config was modified: %+v", fake.got)
	}
}

// If the base config cannot be read we cannot tell which domains are the
// tailnet's. Guessing risks capturing the machine's LAN domain, so the
// wrapper must pass the config through unchanged — same behaviour as before
// the wrapper existed.
func TestSplitDNSLeavesConfigAloneWhenBaseUnreadable(t *testing.T) {
	fake := &fakeOSConfigurator{baseErr: errors.New("no base config")}
	c := newSplitDNSConfigurator(fake)

	if err := c.SetDNS(dns.OSConfig{
		Nameservers:   quad100(t),
		SearchDomains: fqdns(t, "internal", "home"),
	}); err != nil {
		t.Fatalf("SetDNS() error = %v", err)
	}
	if len(fake.got.MatchDomains) != 0 {
		t.Errorf("MatchDomains = %v, want empty (must not guess)", fake.got.MatchDomains)
	}
}

// Every search domain already known to the OS means nothing is ours to claim.
func TestSplitDNSNoTailnetDomains(t *testing.T) {
	fake := &fakeOSConfigurator{base: dns.OSConfig{SearchDomains: fqdns(t, "home")}}
	c := newSplitDNSConfigurator(fake)

	if err := c.SetDNS(dns.OSConfig{
		Nameservers:   quad100(t),
		SearchDomains: fqdns(t, "home"),
	}); err != nil {
		t.Fatalf("SetDNS() error = %v", err)
	}
	if len(fake.got.MatchDomains) != 0 {
		t.Errorf("MatchDomains = %v, want empty", fake.got.MatchDomains)
	}
}

// Errors from the underlying configurator must propagate — a failure to write
// resolver files cannot be swallowed.
func TestSplitDNSPropagatesError(t *testing.T) {
	wantErr := errors.New("write failed")
	fake := &fakeOSConfigurator{
		base:   dns.OSConfig{SearchDomains: fqdns(t, "home")},
		setErr: wantErr,
	}
	c := newSplitDNSConfigurator(fake)

	if err := c.SetDNS(dns.OSConfig{
		Nameservers:   quad100(t),
		SearchDomains: fqdns(t, "internal"),
	}); !errors.Is(err, wantErr) {
		t.Errorf("SetDNS() error = %v, want %v", err, wantErr)
	}
}

func TestSplitDNSNilBase(t *testing.T) {
	if got := newSplitDNSConfigurator(nil); got != nil {
		t.Errorf("newSplitDNSConfigurator(nil) = %v, want nil", got)
	}
}
