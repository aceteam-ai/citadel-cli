//go:build windows

package worklock

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

// Lock is a held single-instance worker lock (Windows).
type Lock struct {
	f    *os.File
	path string
}

// Acquire takes an exclusive, non-blocking lock on the worker lock file derived
// from stateDir using LockFileEx with LOCKFILE_FAIL_IMMEDIATELY. Windows advisory
// locks are released by the OS when the owning handle is closed or the process
// exits, so a crashed worker never leaves a blocking lock.
func Acquire(stateDir string, logf func(format string, args ...any)) (*Lock, error) {
	path := LockPathForStateDir(stateDir)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("worklock: create lock dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("worklock: open lock file %s: %w", path, err)
	}

	handle := windows.Handle(f.Fd())
	var overlapped windows.Overlapped
	// Lock the first byte exclusively, failing immediately if held.
	lockErr := windows.LockFileEx(
		handle,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped,
	)
	if lockErr != nil {
		holderPID := readPID(f)
		_ = f.Close()
		// ERROR_LOCK_VIOLATION (33) is the "already held" signal.
		if lockErr == windows.ERROR_LOCK_VIOLATION {
			return nil, &ErrAlreadyRunning{PID: holderPID, Path: path}
		}
		return nil, fmt.Errorf("worklock: LockFileEx %s: %w", path, lockErr)
	}

	if prevPID := readPID(f); prevPID > 0 && prevPID != os.Getpid() {
		if decideStaleLock(prevPID, os.Getpid(), processAlive(prevPID)) == actionReclaim && logf != nil {
			logf("worklock: reclaimed stale worker lock from dead PID %d (%s)", prevPID, path)
		}
	}

	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
		_ = f.Sync()
	}

	return &Lock{f: f, path: path}, nil
}

// Release relinquishes the lock and removes the lock file.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	handle := windows.Handle(l.f.Fd())
	var overlapped windows.Overlapped
	_ = windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)
	_ = l.f.Close()
	_ = os.Remove(l.path)
	l.f = nil
}

// Path returns the lock file path (for diagnostics).
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

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

// processAlive reports whether a process with the given PID is alive on Windows.
// OpenProcess with QUERY_LIMITED_INFORMATION succeeds for a live process; failure
// (e.g. ERROR_INVALID_PARAMETER for a gone PID) means not alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		// Handle opened but state unknown; treat as alive to be safe (refuse).
		return true
	}
	const stillActive = 259 // STILL_ACTIVE
	return code == stillActive
}
