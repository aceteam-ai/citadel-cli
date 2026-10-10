package update

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

var fixtureCacheDir string

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
	fixtureCacheDir = filepath.Join(dir, "fixtures")
	if err := os.Mkdir(fixtureCacheDir, 0o700); err != nil {
		panic(err)
	}
	for _, k := range []string{"HOME", "LOCALAPPDATA", "APPDATA", "USERPROFILE"} {
		_ = os.Setenv(k, dir)
	}
	// A test may replace HOME with its own t.TempDir. Keep Go's module and build
	// caches independent of that per-test home so compile-only executable
	// fixtures neither redownload the module graph nor leave Go's read-only
	// module tree behind for testing.TempDir cleanup. Explicit caller-owned
	// caches are preserved for hermetic/offline invocations.
	for key, subdir := range map[string]string{
		"GOCACHE":    "go-build-cache",
		"GOMODCACHE": "go-module-cache",
	} {
		if value, ok := os.LookupEnv(key); ok && value != "" {
			continue
		}
		cacheDir := filepath.Join(dir, subdir)
		if err := os.Mkdir(cacheDir, 0o700); err != nil {
			panic(err)
		}
		if err := os.Setenv(key, cacheDir); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr, "update test sandbox cleanup: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
