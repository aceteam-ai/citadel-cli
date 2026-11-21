package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

const podmanUserMarker = "/etc/citadel/rootless-worker-user"

var podmanMarkerDirectoryForTests atomic.Pointer[string]

// podmanUserMarkerPath captures the selected path before marker I/O. The
// production default and its root ownership/mode checks are unchanged.
func podmanUserMarkerPath() string {
	if dir := podmanMarkerDirectoryForTests.Load(); dir != nil {
		return filepath.Join(*dir, "rootless-worker-user")
	}
	return podmanUserMarker
}

// overridePodmanMarkerDirectoryForTesting requires caller-owned fixtures and
// serialized scope changes. Join users before restoring, then remove fixtures.
// This is a path seam, not permission to provision accounts or run services.
func overridePodmanMarkerDirectoryForTesting(dir string) (func() error, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("podman test marker directory must be absolute")
	}
	clean := filepath.Clean(dir)
	info, err := os.Lstat(clean)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("podman test marker directory must be an existing non-symlink directory")
	}
	previous := podmanMarkerDirectoryForTests.Swap(&clean)
	var mu sync.Mutex
	restored := false
	return func() error {
		mu.Lock()
		defer mu.Unlock()
		if restored {
			return nil
		}
		if !podmanMarkerDirectoryForTests.CompareAndSwap(&clean, previous) {
			return errors.New("podman test marker scopes must be restored in reverse order")
		}
		restored = true
		return nil
	}, nil
}
