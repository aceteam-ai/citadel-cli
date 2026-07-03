//go:build !windows

package worklock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Lock is a held single-instance worker lock. Call Release (or rely on process
// exit) to relinquish it.
type Lock struct {
	f    *os.File
	path string
}

// Acquire takes an exclusive, non-blocking advisory lock on the worker lock file
// derived from stateDir. On success it returns a *Lock and writes this process's
// PID into the file for diagnostics.
//
// If another live worker holds the lock, it returns *ErrAlreadyRunning. If the
// lock file exists but its recorded holder PID is dead, the lock is reclaimed
// (flock already guarantees the kernel freed a dead holder's lock; the PID check
// only drives the human-readable log via logf).
//
// logf, if non-nil, receives a single informational line when a stale lock is
// reclaimed. Pass nil to stay silent.
func Acquire(stateDir string, logf func(format string, args ...any)) (*Lock, error) {
	path := LockPathForStateDir(stateDir)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("worklock: create lock dir: %w", err)
	}

	// Open (or create) the lock file. We keep the fd open for the lifetime of the
	// worker; the flock is tied to this open file description and is released by
	// the kernel on close or process exit.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("worklock: open lock file %s: %w", path, err)
	}

	// Non-blocking exclusive lock. If a live process holds it, flock returns
	// EWOULDBLOCK immediately (no hang).
	if lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr != nil {
		// Could not take the lock. Read the recorded holder PID for a clear message.
		holderPID := readPID(f)
		_ = f.Close()
		if errors.Is(lockErr, syscall.EWOULDBLOCK) {
			return nil, &ErrAlreadyRunning{PID: holderPID, Path: path}
		}
		return nil, fmt.Errorf("worklock: flock %s: %w", path, lockErr)
	}

	// We hold the lock. If a stale PID was recorded by a dead prior holder, note
	// the reclaim for the operator. (flock already let us in, so the prior holder
	// is definitively gone; this is purely explanatory.)
	if prevPID := readPID(f); prevPID > 0 && prevPID != os.Getpid() {
		if decideStaleLock(prevPID, os.Getpid(), processAlive(prevPID)) == actionReclaim && logf != nil {
			logf("worklock: reclaimed stale worker lock from dead PID %d (%s)", prevPID, path)
		}
	}

	// Record our PID. Truncate first so a shorter PID never leaves trailing bytes.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
		_ = f.Sync()
	}

	return &Lock{f: f, path: path}, nil
}

// Release relinquishes the lock and removes the lock file. Safe to call more than
// once. The advisory lock is also released automatically on process exit even if
// Release is never called.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	// Remove the file before unlocking/closing so a racing Acquire either sees no
	// file (and creates a fresh one) or blocks on our still-held lock — never
	// observes a half-released lock.
	_ = os.Remove(l.path)
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

// Path returns the lock file path (for diagnostics).
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// readPID reads and parses the PID recorded in an open lock file. Returns 0 if
// the file is empty or unparseable.
func readPID(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	if n <= 0 {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// processAlive reports whether a process with the given PID is currently alive.
// On Unix, signal 0 probes existence without affecting the process: nil error or
// EPERM (exists but not ours to signal) both mean alive; ESRCH means gone.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
