//go:build linux

package platform

import "testing"

// Selected by the existing opt-in Linux real-browser CI, not ordinary tests.
func TestCobrowseRealBrowserSuspendPages(t *testing.T) {
	e := newRealEnv(t)
	runSuspendPagesRealBrowser(t, e.manager())
}
