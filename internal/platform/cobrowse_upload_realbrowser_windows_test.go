//go:build windows

package platform

import (
	"path/filepath"
	"testing"
)

// TestWindowsRealBrowserFileUpload runs the shared upload scenario against the
// real Windows headless runner (Chrome and Edge CI steps). The name matches the
// existing ^TestWindowsRealBrowser run list.
func TestWindowsRealBrowserFileUpload(t *testing.T) {
	requireBrowserIT(t)
	base := filepath.Join(t.TempDir(), "cobrowse-sessions")
	runFileUploadRealBrowser(t, newCobrowseSessionManager(base, 2))
}
