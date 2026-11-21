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

// TestCobrowseRealBrowserFileUploadSpoof: a page that spoofs the in-page URL
// getters is refused and the server receives nothing.
func TestCobrowseRealBrowserFileUploadSpoof(t *testing.T) {
	e := newRealEnv(t)
	runFileUploadSpoofRealBrowser(t, e.manager())
}
