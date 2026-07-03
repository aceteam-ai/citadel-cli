//go:build !windows

package worklock

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Lock is a held single-instance worker lock. Call Release (or rely on process
// exit) to relinquish it.
type Lock struct {
	f    *os.File
	path string
}

// Acquire takes an exclusive, non-blocking advisory lock on the worker lock file
// derived from stateDir. On success it returns a *Lock and writes a JSON record
// (this process's PID, start time, and version) into the file.
//
// Refuse-vs-reclaim uses two layers. The OS advisory lock (flock) is authoritative:
// the kernel frees it on process death, so a crashed worker never wedges its own
// restart. On top of that, an explicit liveness + identity check (is the recorded
// PID alive AND actually a citadel process, via /proc/<pid>/cmdline) guards against
// a reused PID: if the flock is contended but the recorded holder is dead or has
// been reused by an unrelated program, the lock is reclaimed rather than refused.
// If a live citadel process holds it, Acquire returns *ErrAlreadyRunning naming the
// holder PID and start time.
//
// version is stamped into the record (may be empty). logf, if non-nil, receives a
// single informational line when a stale lock is reclaimed; pass nil to stay silent.
func Acquire(stateDir, version string, logf func(format string, args ...any)) (*Lock, error) {
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
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("worklock: flock %s: %w", path, lockErr)
		}
		// The lock is contended. Cross-check the recorded holder before refusing:
		// only a live citadel process is a real duplicate worth refusing. (A reused
		// PID cannot actually hold the flock, but this keeps the refusal message
		// honest and the identity check consistent between paths.)
		rec := readRecord(f)
		_ = f.Close()
		if rec.PID > 0 && processAlive(rec.PID) && processIsCitadel(rec.PID) {
			return nil, &ErrAlreadyRunning{PID: rec.PID, StartTime: startTime(rec), Path: path}
		}
		// Holder is dead / not a citadel process yet the flock is still reported
		// held (rare, e.g. an unrelated fd on the same file). Surface a clear error
		// rather than silently overwriting a lock we do not actually hold.
		return nil, &ErrAlreadyRunning{PID: rec.PID, StartTime: startTime(rec), Path: path}
	}

	// We hold the lock. If a stale record was left by a prior holder, note the
	// reclaim for the operator. flock already let us in, so any prior holder is
	// gone; the identity check keeps the log accurate for a reused PID.
	if prev := readRecord(f); prev.PID > 0 && prev.PID != os.Getpid() {
		if decideStaleLock(prev.PID, os.Getpid(), processAlive(prev.PID), processIsCitadel(prev.PID)) == actionReclaim && logf != nil {
			logf("worklock: reclaimed stale worker lock from PID %d (%s)", prev.PID, path)
		}
	}

	// Record our identity. Truncate first so a shorter record never leaves
	// trailing bytes from a longer prior one.
	if err := f.Truncate(0); err == nil {
		rec := lockRecord{PID: os.Getpid(), StartUnix: time.Now().Unix(), Version: version}
		_, _ = f.WriteAt(encodeRecord(rec), 0)
		_ = f.Sync()
	}

	return &Lock{f: f, path: path}, nil
}

// IsHeld reports whether a live citadel worker currently holds the single-instance
// lock for the node keyed to stateDir, WITHOUT acquiring, reclaiming, or otherwise
// disturbing it. It is the read-only counterpart to Acquire, used by the control
// center to detect a running `citadel work` before deciding whether to run its own
// embedded job worker (issue: control-center competes for per-node-stream jobs).
//
// Detection mirrors Acquire's refuse-vs-reclaim logic but never mutates the lock:
//   - A non-blocking exclusive flock probe on a SEPARATE fd tests contention. The
//     probe fd is unlocked and closed immediately, so a successful probe never
//     leaves the caller holding the lock (it is not the persistent worker lock).
//   - If the probe is contended (EWOULDBLOCK) AND the recorded holder PID is a live
//     citadel process, the lock is reported held with that PID.
//   - A missing file, unparseable record, dead holder, or PID reused by an
//     unrelated program reports NOT held (holderPID 0) — the same conditions under
//     which Acquire would reclaim rather than refuse.
//
// A false "not held" only degrades to the pre-fix behavior (the control center may
// run its own worker); it never steals or breaks the real worker's lock.
func IsHeld(stateDir string) (held bool, holderPID int) {
	path := LockPathForStateDir(stateDir)

	f, err := os.OpenFile(path, os.O_RDONLY, 0o600)
	if err != nil {
		// No lock file (or unreadable): no worker holds it.
		return false, 0
	}
	defer func() { _ = f.Close() }()

	rec := readRecord(f)

	// Non-blocking exclusive probe. If it succeeds, no one holds the lock; unlock
	// immediately so this transient probe is not mistaken for a real worker lock.
	if lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, 0
	} else if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
		// Unexpected flock error: fail open (report not held) so the control center
		// stays functional rather than wedging on a probe error.
		return false, 0
	}

	// Contended. Only a live citadel process counts as a real holder; a dead or
	// reused PID reports not held (Acquire would reclaim it).
	if rec.PID > 0 && processAlive(rec.PID) && processIsCitadel(rec.PID) {
		return true, rec.PID
	}
	return false, 0
}

// startTime converts a record's start timestamp to a time.Time, or the zero value
// when unknown (e.g. a legacy bare-PID lock file with no timestamp).
func startTime(rec lockRecord) time.Time {
	if rec.StartUnix <= 0 {
		return time.Time{}
	}
	return time.Unix(rec.StartUnix, 0)
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

// readRecord reads and parses the lock record from an open lock file. Returns a
// zero record if the file is empty or unparseable. Handles both the JSON record
// format and a legacy bare-PID line.
func readRecord(f *os.File) lockRecord {
	buf := make([]byte, 512)
	n, _ := f.ReadAt(buf, 0)
	if n <= 0 {
		return lockRecord{}
	}
	return decodeRecord(buf[:n])
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

// processIsCitadel reports whether the process with the given PID is a citadel
// process. On Linux it reads /proc/<pid>/cmdline (NUL-separated argv) and checks
// for "citadel". This guards against a reused PID: after a reboot the PID counter
// can wrap onto an unrelated program, and that program must not be mistaken for a
// live worker. On non-Linux Unix (macOS), /proc is unavailable, so this returns
// true (fail closed: keep refusing on an alive PID) — the flock is already the
// authoritative singleton there.
func processIsCitadel(pid int) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		// /proc absent (macOS) or unreadable: fail closed and treat as citadel so
		// an alive holder is still refused; the flock remains the hard guard.
		return true
	}
	// cmdline is NUL-separated; normalize to spaces and lowercase for a robust
	// substring match against the binary name in argv[0] or later args.
	return bytes.Contains(bytes.ToLower(bytes.ReplaceAll(data, []byte{0}, []byte{' '})), []byte("citadel"))
}
