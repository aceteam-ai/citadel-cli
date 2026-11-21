//go:build linux

package platform

import "testing"

func TestCobrowseRealBrowserSigninDisabledAfterProfileRestore(t *testing.T) {
	e := newRealEnv(t)
	runBrowserSigninRealBrowser(t, e.manager())
}
