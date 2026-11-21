//go:build citadel_e2e

// Test-only seam for tests/localgate. It is compiled only with the citadel_e2e
// build tag, which no release build sets. It lets the end-to-end test run the
// real `citadel local mcp` against a page on this machine while the production
// address policy and egress guard stay in force: the page's name resolves to a
// public-looking address, which the policy accepts, and only the egress
// guard's final dial is redirected to the local test server.
package cmd

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
)

const (
	e2eHost     = "gate.test"
	e2eFakeAddr = "198.51.100.7" // TEST-NET-2: not blocked as private or reserved
)

func init() {
	pageAddr := os.Getenv("CITADEL_E2E_PAGE_ADDR")
	openedFile := os.Getenv("CITADEL_E2E_OPENED_FILE")
	if pageAddr == "" || openedFile == "" {
		return
	}
	localE2EHook = func(o *localCobrowseOptions) {
		o.resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
			if strings.EqualFold(host, e2eHost) {
				return []netip.Addr{netip.MustParseAddr(e2eFakeAddr)}, nil
			}
			return nil, fmt.Errorf("e2e resolver: unknown host %q", host)
		}
		o.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil || host != e2eFakeAddr {
				return nil, fmt.Errorf("e2e dialer: refusing %q", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, pageAddr)
		}
		// Record the launch file instead of opening a desktop browser.
		o.openURL = func(path string) error {
			f, err := os.OpenFile(openedFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = fmt.Fprintln(f, path)
			return err
		}
	}
}
