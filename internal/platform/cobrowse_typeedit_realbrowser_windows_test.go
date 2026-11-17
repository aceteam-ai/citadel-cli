//go:build windows

package platform

import (
	"path/filepath"
	"testing"
)

// TestWindowsRealBrowserTypeEdit runs the shared type-replace and key-modifier
// scenario against the real Windows headless runner (Chrome and Edge CI steps).
// The name matches the existing ^TestWindowsRealBrowser run list.
func TestWindowsRealBrowserTypeEdit(t *testing.T) {
	requireBrowserIT(t)
	base := filepath.Join(t.TempDir(), "cobrowse-sessions")
	runTypeEditRealBrowser(t, newCobrowseSessionManager(base, 2))
}
