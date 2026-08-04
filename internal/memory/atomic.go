package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const lockWait = 3 * time.Second

// withFileLock serializes Citadel read-modify-write operations across
// processes. The platform implementation uses a crash-releasing kernel lock
// (flock on Unix, LockFileEx on Windows) on a persistent sidecar. The sidecar
// is deliberately never deleted: deleting lock files creates a pathname race
// where different writers can lock different inodes. Locking the destination
// inode itself would not work because atomicWriteFile replaces that inode.
func withFileLock(path string, fn func() error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create lock directory: %w", err)
	}
	lockPath := path + ".citadel.lock"
	f, err := openLockFile(lockPath)
	if err != nil {
		return fmt.Errorf("open config lock: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("config lock is not a regular file")
	}
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("secure config lock permissions: %w", err)
	}
	if err := lockFile(f, time.Now().Add(lockWait)); err != nil {
		return fmt.Errorf("acquire config lock %s: %w", lockPath, err)
	}
	defer unlockFile(f)
	return fn()
}

// atomicWriteFile writes data through a random, user-only temporary file in
// the destination directory and renames it into place. Same-directory rename
// keeps the replacement atomic; a random CreateTemp name avoids predictable
// symlink targets. The temporary file is always created 0600, even when the
// final file may be less restrictive.
func atomicWriteFile(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if err = tmp.Chmod(perm); err != nil {
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace destination: %w", err)
	}
	return nil
}
