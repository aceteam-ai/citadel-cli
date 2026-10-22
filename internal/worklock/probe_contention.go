package worklock

import (
	"errors"
	"os"
)

var errContentionProbeUnavailable = errors.New("worker lock contention unavailable")

// ProbeContention observes the existing node lock without creating, repairing,
// or reading its record. Any kernel contention counts as held, independently of
// a recorded PID. Only false, nil means no contention was observed. This is a
// snapshot, not ownership authority: callers starting a worker still need Acquire.
// Missing paths are normal only at the initial observation. Unexpected errors
// are deliberately opaque and must not be interpreted as absence.
func ProbeContention(stateDir string) (bool, error) {
	return probeContention(LockPathForStateDir(stateDir), nativeContentionProbeOps())
}

type contentionProbeFile interface {
	Stat() (os.FileInfo, error)
	Fd() uintptr
	Close() error
}

// Per-call operations keep fixtures independent without replacing global hooks.
type contentionProbeOps struct {
	lstat       func(string) (os.FileInfo, error)
	open        func(string) (contentionProbeFile, error)
	sameFile    func(os.FileInfo, os.FileInfo) bool
	lock        func(contentionProbeFile) error
	unlock      func(contentionProbeFile) error
	isContended func(error) bool
}

func probeContention(path string, ops contentionProbeOps) (bool, error) {
	before, err := ops.lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || before == nil || !before.Mode().IsRegular() {
		return false, errContentionProbeUnavailable
	}
	file, err := ops.open(path)
	if err != nil {
		return false, errContentionProbeUnavailable
	}
	finish := func(held bool, failure bool) (bool, error) {
		if file.Close() != nil || failure {
			return false, errContentionProbeUnavailable
		}
		return held, nil
	}
	opened, err := file.Stat()
	if err != nil || opened == nil || !opened.Mode().IsRegular() || !ops.sameFile(before, opened) {
		return finish(false, true)
	}
	lockErr := ops.lock(file)
	held := lockErr != nil && ops.isContended(lockErr)
	if lockErr != nil && !held {
		return finish(false, true)
	}
	// Recheck the path even on contention; a lock on a substituted inode says
	// nothing about the node path. Never follow a replacement final symlink.
	after, statErr := ops.lstat(path)
	identityOK := statErr == nil && after != nil && after.Mode().IsRegular() && ops.sameFile(opened, after)
	if lockErr == nil {
		// Always attempt to release a successful probe, even after substitution.
		if ops.unlock(file) != nil {
			return finish(false, true)
		}
	}
	return finish(held, !identityOK)
}
