//go:build linux

package platform

import "testing"

// TestCobrowseRealBrowserFileUpload runs the shared upload scenario against the
// real Linux runner. Opt-in like the other real-browser tests; the name matches
// the CI job's ^TestCobrowseRealBrowser list.
func TestCobrowseRealBrowserFileUpload(t *testing.T) {
	e := newRealEnv(t)
	runFileUploadRealBrowser(t, e.manager())
}
