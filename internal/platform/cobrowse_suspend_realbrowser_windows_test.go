//go:build windows

package platform

import "testing"

// Existing opt-in Windows real-browser CI runs this for Chrome and Edge.
func TestWindowsRealBrowserSuspendPages(t *testing.T) {
	requireBrowserIT(t)
	runSuspendPagesRealBrowser(t, newCobrowseSessionManager(trustedBaseDir(t), 2))
}
