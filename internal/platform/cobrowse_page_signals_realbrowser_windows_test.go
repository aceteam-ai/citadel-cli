//go:build windows

package platform

import (
	"path/filepath"
	"testing"
)

// TestWindowsRealBrowserPageSignals runs the shared page_text/password_field
// scenario against the real Windows headless runner (Chrome and Edge CI steps).
func TestWindowsRealBrowserPageSignals(t *testing.T) {
	requireBrowserIT(t)
	base := filepath.Join(t.TempDir(), "cobrowse-sessions")
	runPageSignalsRealBrowser(t, newCobrowseSessionManager(base, 2))
}
