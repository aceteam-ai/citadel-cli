package update

import (
	"os"
	"testing"
)

// TestMain sandboxes the home directory for the ENTIRE update package test run
// so no test (new or pre-existing) ever reads or writes the real
// ~/citadel-node/update/state.json. State paths resolve from HOME (POSIX) or
// LOCALAPPDATA/APPDATA/USERPROFILE (Windows) via getUserHomeDir, so all are
// redirected. This matters on a dev box that runs a live node: the apply/result
// write paths in runOnce (citadel-cli#1134) and the long-standing happy-path
// SaveState would otherwise clobber the operator's own update state. Individual
// tests that need their own temp dir still override HOME per-test; this is only
// the backstop for those that do not.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "citadel-update-test-home-")
	if err != nil {
		panic(err)
	}
	for _, k := range []string{"HOME", "LOCALAPPDATA", "APPDATA", "USERPROFILE"} {
		_ = os.Setenv(k, dir)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
