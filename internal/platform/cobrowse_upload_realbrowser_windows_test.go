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

// TestWindowsRealBrowserFileUploadSpoof is the negative control (see the
// shared scenario); its name matches the existing run list prefix.
func TestWindowsRealBrowserFileUploadSpoof(t *testing.T) {
	requireBrowserIT(t)
	base := filepath.Join(t.TempDir(), "cobrowse-sessions")
	runFileUploadSpoofRealBrowser(t, newCobrowseSessionManager(base, 2))
}
