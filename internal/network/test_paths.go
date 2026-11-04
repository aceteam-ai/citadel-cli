package network

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

type testStateDirectories struct {
	node, machine string
}

var stateDirectoriesForTests atomic.Pointer[testStateDirectories]

// OverrideStateDirectoriesForTesting confines default network state and machine
// pointer discovery to caller-owned fixture directories. Install it before any
// path-dependent work; stop and join all users before restoring or removing the
// fixtures. Readers are concurrency-safe, but tests must serialize scope changes
// and restore nested scopes in reverse order. This is not a filesystem sandbox.
// There is no environment switch and production resolution is unchanged unless
// a test explicitly installs this override.
func OverrideStateDirectoriesForTesting(nodeDir, machineDir string) (func() error, error) {
	node, err := fixtureDirectory(nodeDir)
	if err != nil {
		return nil, err
	}
	machine, err := fixtureDirectory(machineDir)
	if err != nil {
		return nil, err
	}
	next := &testStateDirectories{node: node, machine: machine}
	previous := stateDirectoriesForTests.Swap(next)
	var mu sync.Mutex
	restored := false
	return func() error {
		mu.Lock()
		defer mu.Unlock()
		if restored {
			return nil
		}
		if !stateDirectoriesForTests.CompareAndSwap(next, previous) {
			return errors.New("network test directory scopes must be restored in reverse order")
		}
		restored = true
		return nil
	}, nil
}

func fixtureDirectory(dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", errors.New("network test directory must be absolute")
	}
	clean := filepath.Clean(dir)
	info, err := os.Lstat(clean)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("network test directory must be an existing non-symlink directory")
	}
	return clean, nil
}
