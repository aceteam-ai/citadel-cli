//go:build linux

package platform

import "testing"

// TestCobrowseRealBrowserEgressGuard runs the public-only scenario against the
// real Linux runner. Opt-in like the other real-browser tests; the name
// matches the CI job's ^TestCobrowseRealBrowser list.
func TestCobrowseRealBrowserEgressGuard(t *testing.T) {
	e := newRealEnv(t)
	runEgressGuardRealBrowser(t, e.manager())
}
