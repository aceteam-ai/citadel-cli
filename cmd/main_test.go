package cmd

import (
	"fmt"
	"os"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// wiredLegacyPasscodeLockout is the production config.LegacyPasscodeLockout
// hook this package's init installed, kept so the tests that exercise it can
// reinstall it against a per-test directory.
var wiredLegacyPasscodeLockout func() config.PasscodeAttemptLimiter

// TestMain keeps the passcode gate hooks off this machine's node state.
// network.GetNodeConfigDir() can resolve to a live node's directory on a
// development machine, so the hooks are pointed at a temporary directory, and
// the node-wide legacy passcode lockout (#1398) is unwired by default so
// unrelated tests that present a wrong passcode neither write lockout state nor
// accumulate failures that lock out later tests. Tests for the lockout wiring
// reinstall it with useWiredLegacyLockout.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "citadel-cmd-passcode-hooks-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: temp dir: %v\n", err)
		os.Exit(1)
	}
	passcodeHookNodeDir = func() string { return dir }
	wiredLegacyPasscodeLockout = config.LegacyPasscodeLockout
	config.LegacyPasscodeLockout = nil
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// useWiredLegacyLockout installs the production passcode hooks against a fresh
// per-test node directory and returns it. Callers must not run in parallel.
func useWiredLegacyLockout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prevDir, prevLockout := passcodeHookNodeDir, config.LegacyPasscodeLockout
	passcodeHookNodeDir = func() string { return dir }
	config.LegacyPasscodeLockout = wiredLegacyPasscodeLockout
	t.Cleanup(func() {
		passcodeHookNodeDir = prevDir
		config.LegacyPasscodeLockout = prevLockout
	})
	return dir
}
