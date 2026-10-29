package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var stateMu sync.Mutex

const stateLockTimeout = 10 * time.Second

// The persistent sibling is never removed or truncated. Locking state.json
// itself would cease to serialize writers as soon as a rename replaced it.
func withStateLock(path string, fn func() error) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	lockPath := path + ".lock"
	file, err := openStateLock(lockPath)
	if err != nil {
		return fmt.Errorf("open update state lock: %w", err)
	}
	defer file.Close()
	if err := validateStateLock(file, lockPath); err != nil {
		return err
	}
	if err := keepStateOwner(file, path); err != nil {
		return err
	}
	if err := file.Chmod(0600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), stateLockTimeout)
	defer cancel()
	for {
		locked, err := tryStateLock(file)
		if err != nil {
			return fmt.Errorf("lock update state: %w", err)
		}
		if locked {
			defer unlockStateFile(file)
			if err := validateStateLock(file, lockPath); err != nil {
				return err
			}
			return fn()
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("timed out waiting for update state lock: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func validateStateLock(file *os.File, path string) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return errors.New("update state lock is not a stable regular file")
	}
	return nil
}

type stateFileOps struct {
	syncFile func(*os.File) error
	rename   func(string, string) error
	syncDir  func(string) error
}

func defaultStateFileOps() stateFileOps {
	return stateFileOps{
		syncFile: func(file *os.File) error { return file.Sync() },
		rename:   replaceStateFile,
		syncDir:  syncStateDir,
	}
}

// Call only while holding the sibling lock. Readers need no lock: a complete
// old or new snapshot is published by the same-directory rename.
func writeStateFile(path string, state *State, ops stateFileOps) error {
	if state == nil {
		return errors.New("cannot save nil update state")
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state.json.tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	mode := os.FileMode(0644)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("update state is not a regular file")
		}
		mode = info.Mode().Perm() & 0666
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := keepStateOwner(tmp, path); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := ops.syncFile(tmp); err != nil {
		return fmt.Errorf("sync update state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := ops.rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace update state: %w", err)
	}
	if err := ops.syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("update state replaced; durability unknown: %w", err)
	}
	return nil
}
