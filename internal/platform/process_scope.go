package platform

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// processDirectories is the production counterpart of the test fixtures in
// test_paths.go. A job-scoped profile (the HPC profile, aceteam#12715) installs
// it once, before anything resolves a path, so ConfigDir, ConfigDirCandidates
// and HomeDir never consult the invoking user's real home, SUDO_USER or the
// machine-wide /etc/citadel for the rest of the process.
type processDirectories struct {
	config, home string
}

var (
	processDirectoryScope atomic.Pointer[processDirectories]
	processScopeMu        sync.Mutex
)

// ScopeProcessDirectories confines ConfigDir, ConfigDirCandidates and HomeDir
// (for every user name) to the given absolute, existing, non-symlink
// directories for the remaining lifetime of this process. It is one-shot: a
// later call fails unless it names exactly the same directories, so a scope
// can never be swapped out from under code that already resolved a path.
// Explicit test fixtures still take precedence, so package tests keep their
// existing isolation.
func ScopeProcessDirectories(configDir, homeDir string) error {
	cfg, err := scopeDirectory(configDir)
	if err != nil {
		return err
	}
	home, err := scopeDirectory(homeDir)
	if err != nil {
		return err
	}
	processScopeMu.Lock()
	defer processScopeMu.Unlock()
	if current := processDirectoryScope.Load(); current != nil {
		if current.config == cfg && current.home == home {
			return nil
		}
		return errors.New("platform process directories are already scoped to a different location")
	}
	processDirectoryScope.Store(&processDirectories{config: cfg, home: home})
	return nil
}

// ProcessDirectoriesScoped reports whether ScopeProcessDirectories is in effect.
func ProcessDirectoriesScoped() bool {
	return processDirectoryScope.Load() != nil
}

func scopedConfigDir() (string, bool) {
	if s := processDirectoryScope.Load(); s != nil {
		return s.config, true
	}
	return "", false
}

func scopedHomeDir() (string, bool) {
	if s := processDirectoryScope.Load(); s != nil {
		return s.home, true
	}
	return "", false
}

func scopeDirectory(dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", errors.New("platform process directory must be absolute")
	}
	clean := filepath.Clean(dir)
	info, err := os.Lstat(clean)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("platform process directory must be an existing non-symlink directory")
	}
	return clean, nil
}
