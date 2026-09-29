//go:build windows

package terminal

import "testing"

func TestCurrentPlatformPersistentTmuxIsDisabledOnWindows(t *testing.T) {
	if currentPlatformPersistentTmuxSupported() {
		t.Fatal("production Windows platform gate unexpectedly enables persistent tmux")
	}
}
