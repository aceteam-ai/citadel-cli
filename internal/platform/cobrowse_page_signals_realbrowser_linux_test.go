//go:build linux

package platform

import "testing"

// TestCobrowseRealBrowserPageSignals runs the shared page_text/password_field
// scenario against the real Linux runner. The name matches the CI job's
// ^TestCobrowseRealBrowser list.
func TestCobrowseRealBrowserPageSignals(t *testing.T) {
	e := newRealEnv(t)
	runPageSignalsRealBrowser(t, e.manager())
}
