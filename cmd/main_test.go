package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/clilog"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// wiredLegacyPasscodeLockout is the production config.LegacyPasscodeLockout
// hook this package's init installed, kept so the tests that exercise it can
// reinstall it against a per-test directory.
var wiredLegacyPasscodeLockout func() config.PasscodeAttemptLimiter

// TestMain redirects the cmd credential reader/writer and passcode hooks, plus
// default invoker-scoped config, into owned temporary paths. This is not a
// sandbox for arbitrary paths or native operations. Network state, machine
// pointer and platform config discovery are explicitly redirected before tests
// run, independently of later HOME changes. Tests that need different config
// fixtures must install their own scoped override. No native execution is authorized by
// this setup. Legacy passcode lockout is unwired unless explicitly reinstalled.
func TestMain(m *testing.M) {
	// A direct init-time assignment would otherwise be overwritten by the
	// Once below, masking its pre-fixture reads and captured paths.
	if jobHandlers != nil {
		fmt.Fprintln(os.Stderr, "TestMain: diagnostic registry initialized before fixture selection")
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "citadel-cmd-fixtures-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: temp dir: %v\n", err)
		os.Exit(1)
	}
	var restorePaths func() error
	failSetup := func(err error) {
		fmt.Fprintf(os.Stderr, "TestMain: fixture setup: %v\n", err)
		if restorePaths != nil {
			if restoreErr := restorePaths(); restoreErr != nil {
				fmt.Fprintf(os.Stderr, "TestMain: path restoration: %v\n", restoreErr)
				os.Exit(1) // Keep directories while a scope still owns them.
			}
		}
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "TestMain: fixture cleanup: %v\n", cleanupErr)
		}
		os.Exit(1)
	}
	home := filepath.Join(dir, "home")
	nodeDir := filepath.Join(dir, "node")
	passcodeDir := filepath.Join(dir, "passcodes")
	machineDir := filepath.Join(dir, "machine")
	markerDir := filepath.Join(dir, "podman-marker")
	legacyDir := filepath.Join(home, ".citadel-cli")
	for _, child := range []string{home, nodeDir, passcodeDir, machineDir, legacyDir, markerDir} {
		if err := os.MkdirAll(child, 0700); err != nil {
			failSetup(err)
		}
	}
	restorePaths, err = installCmdPathFixtures(home, nodeDir, machineDir, legacyDir, markerDir)
	if err != nil {
		failSetup(err)
	}
	// Preflight path selection before any logger, credential or test helper can
	// create files. Do not use a production writer to establish this oracle.
	if err := checkCmdPathFixtures(home, nodeDir, legacyDir, markerDir); err != nil {
		failSetup(err)
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
	nodeConfigDirFn = network.GetNodeConfigDir
	fixStatePermissionsFn = func() {}
	passcodeHookNodeDir = func() string { return passcodeDir }
	wiredLegacyPasscodeLockout = config.LegacyPasscodeLockout
	config.LegacyPasscodeLockout = nil
	// The diagnostic registry used to read/capture host paths in init(), before
	// TestMain. Construct it only after all path providers are installed.
	ensureDiagnosticJobHandlers()
	code := m.Run()
	// Windows does not allow deleting a directory containing an open log file.
	if err := clilog.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: log cleanup: %v\n", err)
		code = 1
	}
	if err := restorePaths(); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: path restoration: %v\n", err)
		code = 1 // Do not remove fixtures still owned by another scope.
	} else if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: fixture cleanup: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

// installCmdPathFixtures changes only default path discovery, not explicit
// paths passed by tests. The caller owns the directories and must join their
// users, close handles, restore this scope, then delete the fixtures.
func installCmdPathFixtures(home, nodeDir, machineDir, configDir, markerDir string) (func() error, error) {
	restoreNetwork, err := network.OverrideStateDirectoriesForTesting(nodeDir, machineDir)
	if err != nil {
		return nil, err
	}
	restorePlatform, err := platform.OverrideConfigDirectoryForTesting(configDir)
	if err != nil {
		if restoreErr := restoreNetwork(); restoreErr != nil {
			return restoreNetwork, fmt.Errorf("platform fixture: %v; network restoration: %w", err, restoreErr)
		}
		return nil, err
	}
	restoreHome, err := platform.OverrideHomeDirectoryForTesting(home)
	if err != nil {
		cleanup := func() error {
			if err := restorePlatform(); err != nil {
				return err
			}
			return restoreNetwork()
		}
		if restoreErr := cleanup(); restoreErr != nil {
			return cleanup, fmt.Errorf("home fixture: %v; path restoration: %w", err, restoreErr)
		}
		return nil, err
	}
	restoreMarker, err := overridePodmanMarkerDirectoryForTesting(markerDir)
	if err != nil {
		cleanup := func() error {
			if err := restoreHome(); err != nil {
				return err
			}
			if err := restorePlatform(); err != nil {
				return err
			}
			return restoreNetwork()
		}
		if restoreErr := cleanup(); restoreErr != nil {
			return cleanup, fmt.Errorf("marker fixture: %v; path restoration: %w", err, restoreErr)
		}
		return nil, err
	}
	return func() error {
		if err := restoreMarker(); err != nil {
			return err
		}
		if err := restoreHome(); err != nil {
			return err
		}
		if err := restorePlatform(); err != nil {
			return err
		}
		return restoreNetwork()
	}, nil
}

// useCmdFixtureHome makes a per-test HOME fixture explicit to both path
// providers. Changing HOME alone must never disable TestMain's host guard.
// Like t.Setenv, this helper must not be used by parallel tests.
func useCmdFixtureHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// Do not pre-create the production fallback home/citadel-node: manifest
	// tests use its absence to prove an explicit node override was honored.
	node := filepath.Join(home, "network-node-fixture")
	machine := filepath.Join(home, "machine-config")
	configDir := filepath.Join(home, ".citadel-cli")
	markerDir := filepath.Join(home, "podman-marker")
	for _, dir := range []string{node, machine, configDir, markerDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := clilog.Close(); err != nil {
		t.Fatal(err)
	}
	restore, err := installCmdPathFixtures(home, node, machine, configDir, markerDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := clilog.Close(); err != nil {
			t.Error(err)
		}
		if err := restore(); err != nil {
			t.Error(err)
		}
	})
	if err := checkCmdPathFixtures(home, node, configDir, markerDir); err != nil {
		t.Fatal(err)
	}
}

// checkCmdPathFixtures compares strings only. Its expected inputs come from
// explicitly created fixtures, not from another default-path getter.
func checkCmdPathFixtures(home, node, configDir, markerDir string) error {
	for _, name := range []string{"", "fixture-account-that-must-not-be-looked-up"} {
		if got, err := platform.HomeDir(name); err != nil || got != home {
			return fmt.Errorf("home fixture not selected for %q", name)
		}
	}
	if network.GetNodeConfigDir() != node || network.GetStateDir() != filepath.Join(node, "network") {
		return fmt.Errorf("network fixture not selected")
	}
	if platform.ConfigDir() != configDir || clilog.Dir() != filepath.Join(home, ".citadel-cli", "logs") {
		return fmt.Errorf("config/log fixture not selected")
	}
	if podmanUserMarkerPath() != filepath.Join(markerDir, "rootless-worker-user") {
		return fmt.Errorf("podman marker fixture not selected")
	}
	return nil
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
