package platform

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

var configDirectoryForTests atomic.Pointer[string]
var homeDirectoryForTests atomic.Pointer[string]

// OverrideConfigDirectoryForTesting selects one caller-owned fixture directory
// for ConfigDir and ConfigDirCandidates, independent of HOME, sudo and OS path
// selectors. Install before path-dependent work and join its users before
// restoring/removing it. Scope changes must be serialized; nested scopes restore
// in reverse order. This explicit test seam is not an environment override or
// an OS sandbox, and does not change the default production resolver.
func OverrideConfigDirectoryForTesting(dir string) (func() error, error) {
	return overrideDirectoryForTesting(&configDirectoryForTests, dir, "config")
}

// OverrideHomeDirectoryForTesting selects a caller-owned home before any
// environment or account lookup in HomeDir, including named/sudo users. It
// follows the same lifetime and custody rules as the config fixture. It does
// not redirect os.UserHomeDir or explicit paths in other packages/processes.
func OverrideHomeDirectoryForTesting(dir string) (func() error, error) {
	return overrideDirectoryForTesting(&homeDirectoryForTests, dir, "home")
}

func overrideDirectoryForTesting(slot *atomic.Pointer[string], dir, kind string) (func() error, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("platform test " + kind + " directory must be absolute")
	}
	clean := filepath.Clean(dir)
	info, err := os.Lstat(clean)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("platform test " + kind + " directory must be an existing non-symlink directory")
	}
	previous := slot.Swap(&clean)
	var mu sync.Mutex
	restored := false
	return func() error {
		mu.Lock()
		defer mu.Unlock()
		if restored {
			return nil
		}
		if !slot.CompareAndSwap(&clean, previous) {
			return errors.New("platform test directory scopes must be restored in reverse order")
		}
		restored = true
		return nil
	}, nil
}
