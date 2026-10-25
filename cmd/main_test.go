package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/clilog"
	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// wiredLegacyPasscodeLockout is the production config.LegacyPasscodeLockout
// hook this package's init installed, kept so the tests that exercise it can
// reinstall it against a per-test directory.
var wiredLegacyPasscodeLockout func() config.PasscodeAttemptLimiter

// TestMain redirects the cmd credential reader/writer and passcode hooks, plus
// default invoker-scoped config, into owned temporary paths. This is not a
// sandbox for direct network.GetNodeConfigDir/GetStateDir or machine pointer
// access: those fixed machine paths bypass HOME and remain outside this guard.
// Tests that deliberately replace HOME or other selectors must still redirect
// every relevant seam themselves. No native command execution is authorized by
// this setup. Legacy passcode lockout is unwired unless explicitly reinstalled.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "citadel-cmd-fixtures-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: temp dir: %v\n", err)
		os.Exit(1)
	}
	failSetup := func(err error) {
		fmt.Fprintf(os.Stderr, "TestMain: fixture setup: %v\n", err)
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "TestMain: fixture cleanup: %v\n", cleanupErr)
		}
		os.Exit(1)
	}
	home := filepath.Join(dir, "home")
	nodeDir := filepath.Join(dir, "node")
	passcodeDir := filepath.Join(dir, "passcodes")
	legacyDir := filepath.Join(home, ".citadel-cli")
	for _, child := range []string{home, nodeDir, passcodeDir, legacyDir, filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(child, 0700); err != nil {
			failSetup(err)
		}
	}
	// platform.ConfigDir's root branch needs an existing HOME config; HOME
	// alone otherwise falls through to the system-wide config directory.
	if err := os.WriteFile(filepath.Join(legacyDir, "config.yaml"), []byte("{}\n"), 0600); err != nil {
		failSetup(err)
	}
	for _, setting := range []struct {
		key   string
		value string
	}{
		{"HOME", home},
		{"USERPROFILE", home},
		{"APPDATA", filepath.Join(dir, "appdata")},
		{"LOCALAPPDATA", filepath.Join(dir, "localappdata")},
		{"ProgramData", filepath.Join(dir, "program-data")},
		{"XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config")},
		{"XDG_DATA_HOME", filepath.Join(dir, "xdg-data")},
		{"XDG_STATE_HOME", filepath.Join(dir, "xdg-state")},
		{"XDG_CACHE_HOME", filepath.Join(dir, "xdg-cache")},
		{"XDG_RUNTIME_DIR", filepath.Join(dir, "xdg-runtime")},
		{"TMPDIR", filepath.Join(dir, "tmp")},
		{"TMP", filepath.Join(dir, "tmp")},
		{"TEMP", filepath.Join(dir, "tmp")},
	} {
		if err := os.MkdirAll(setting.value, 0700); err != nil {
			failSetup(err)
		}
		if err := os.Setenv(setting.key, setting.value); err != nil {
			failSetup(err)
		}
	}
	for _, key := range []string{"SUDO_USER", "CITADEL_NODE_DIR"} {
		if err := os.Unsetenv(key); err != nil {
			failSetup(err)
		}
	}
	nodeConfigDirFn = func() string { return nodeDir }
	fixStatePermissionsFn = func() {}
	passcodeHookNodeDir = func() string { return passcodeDir }
	wiredLegacyPasscodeLockout = config.LegacyPasscodeLockout
	config.LegacyPasscodeLockout = nil
	code := m.Run()
	// Windows does not allow deleting a directory containing an open log file.
	if err := clilog.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: log cleanup: %v\n", err)
		code = 1
	}
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: fixture cleanup: %v\n", err)
		code = 1
	}
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
