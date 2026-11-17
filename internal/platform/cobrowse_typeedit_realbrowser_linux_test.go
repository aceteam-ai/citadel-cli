//go:build linux

package platform

import "testing"

// TestCobrowseRealBrowserTypeEdit runs the shared type-replace and key-modifier
// scenario against the real Linux runner. Opt-in like the other real-browser
// tests; the name matches the CI job's ^TestCobrowseRealBrowser list.
func TestCobrowseRealBrowserTypeEdit(t *testing.T) {
	e := newRealEnv(t)
	runTypeEditRealBrowser(t, e.manager())
}
