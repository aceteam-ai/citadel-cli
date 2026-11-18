//go:build windows

package platform

import (
	"path/filepath"
	"testing"
)

func TestWindowsRealBrowserSigninDisabledAfterProfileRestore(t *testing.T) {
	requireBrowserIT(t)
	runBrowserSigninRealBrowser(t, newCobrowseSessionManager(filepath.Join(t.TempDir(), "sessions"), 2))
}
