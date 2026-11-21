// Package-local compatibility names keep navigation and egress on one policy.
package localcobrowse

import (
	"context"
	"net/netip"

	"github.com/aceteam-ai/citadel-cli/internal/browserurl"
)

var ErrNavigationRefused = browserurl.ErrNavigationRefused

type Resolver = browserurl.Resolver
type URLPolicy = browserurl.URLPolicy

func DefaultResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	return browserurl.DefaultResolver(ctx, host)
}
func NewURLPolicy(resolve Resolver, ports func() []int) URLPolicy {
	return browserurl.NewURLPolicy(resolve, ports)
}
func blockedAddr(a netip.Addr) bool                   { return browserurl.BlockedAddr(a) }
func parseBrowserHost(host string) (netip.Addr, bool) { return browserurl.ParseBrowserHost(host) }
func looksNumericHost(host string) bool               { return browserurl.LooksNumericHost(host) }
