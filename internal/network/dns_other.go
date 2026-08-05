//go:build !darwin

// internal/network/dns_other.go
package network

import "tailscale.com/net/dns"

// newSplitDNSConfigurator is a no-op off macOS. Linux (systemd-resolved) and
// Windows (NRPT) both apply the manager's config faithfully, including the
// global-resolver form; only macOS's /etc/resolver cannot express it. See
// dns_darwin.go and issue #676.
func newSplitDNSConfigurator(base dns.OSConfigurator) dns.OSConfigurator {
	return base
}
