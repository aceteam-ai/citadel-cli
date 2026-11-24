//go:build windows

package platform

import (
	"path/filepath"
	"testing"
)

// TestWindowsRealBrowserEgressGuard runs the public-only scenario against the
// real Windows headless runner (Chrome and Edge CI steps). The name matches
// the existing ^TestWindowsRealBrowser run list. Windows keeps no session
// registry, so an isolated manager with the public-only source set takes the
// same launch path as the node-wide manager.
func TestWindowsRealBrowserEgressGuard(t *testing.T) {
	requireBrowserIT(t)
	base := filepath.Join(t.TempDir(), "cobrowse-sessions")
	runEgressGuardRealBrowser(t, newCobrowseSessionManager(base, 2))
}
