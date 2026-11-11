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

// processStateDirectories is the production counterpart of the test fixture
// above, installed once by a job-scoped profile (the HPC profile,
// aceteam#12715) before any path is resolved. It shares the fixture's
// semantics: the node dir replaces every pointer, global-config and
// owner-home lookup, and the machine dir replaces the machine-wide config dir
// (so /etc/citadel is never read).
var (
	processStateDirectories atomic.Pointer[testStateDirectories]
	processStateMu          sync.Mutex
)

// ScopeProcessStateDirectories confines network state, node config and
// machine pointer discovery to the given absolute, existing, non-symlink
// directories for the remaining lifetime of this process. It is one-shot: a
// later call fails unless it names exactly the same directories. A test
// fixture installed with OverrideStateDirectoriesForTesting still wins.
func ScopeProcessStateDirectories(nodeDir, machineDir string) error {
	node, err := fixtureDirectory(nodeDir)
	if err != nil {
		return err
	}
	machine, err := fixtureDirectory(machineDir)
	if err != nil {
		return err
	}
	processStateMu.Lock()
	defer processStateMu.Unlock()
	if current := processStateDirectories.Load(); current != nil {
		if current.node == node && current.machine == machine {
			return nil
		}
		return errors.New("network process state directories are already scoped to a different location")
	}
	processStateDirectories.Store(&testStateDirectories{node: node, machine: machine})
	return nil
}

// ProcessStateDirectoriesScoped reports whether ScopeProcessStateDirectories
// is in effect.
func ProcessStateDirectoriesScoped() bool {
	return processStateDirectories.Load() != nil
}

// stateScope returns the active directory scope: a test fixture first, then a
// production process scope, else nil (ordinary machine resolution).
func stateScope() *testStateDirectories {
	if fixture := stateDirectoriesForTests.Load(); fixture != nil {
		return fixture
	}
	return processStateDirectories.Load()
}
