package platform

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

var configDirectoryForTests atomic.Pointer[string]

// OverrideConfigDirectoryForTesting selects one caller-owned fixture directory
// for ConfigDir and ConfigDirCandidates, independent of HOME, sudo and OS path
// selectors. Install before path-dependent work and join its users before
// restoring/removing it. Scope changes must be serialized; nested scopes restore
// in reverse order. This explicit test seam is not an environment override or
// an OS sandbox, and does not change the default production resolver.
func OverrideConfigDirectoryForTesting(dir string) (func() error, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("platform test config directory must be absolute")
	}
	clean := filepath.Clean(dir)
	info, err := os.Lstat(clean)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("platform test config directory must be an existing non-symlink directory")
	}
	previous := configDirectoryForTests.Swap(&clean)
	var mu sync.Mutex
	restored := false
	return func() error {
		mu.Lock()
		defer mu.Unlock()
		if restored {
			return nil
		}
		if !configDirectoryForTests.CompareAndSwap(&clean, previous) {
			return errors.New("platform test directory scopes must be restored in reverse order")
		}
		restored = true
		return nil
	}, nil
}
