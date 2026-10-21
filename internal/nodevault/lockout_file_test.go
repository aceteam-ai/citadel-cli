//go:build linux || darwin || windows

package nodevault

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// This helper is reached only in a new test-binary process, with a disposable
// directory supplied by its parent. It never opens a real node/vault.
func TestLockoutProcessHelper(t *testing.T) {
	mode := os.Getenv("CITADEL_LOCKOUT_TEST_MODE")
	if mode == "" {
		return
	}
	path := os.Getenv("CITADEL_LOCKOUT_TEST_PATH")
	if mode == "hold" {
		file, err := openLockoutFile(path + ".lock")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := lockLockoutFile(file, time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		defer unlockLockoutFile(file)
	} else if mode != "transactions" {
		t.Fatal("unknown helper mode")
	}
	fmt.Fprintln(os.Stdout, "ready")
	var start [1]byte
	if _, err := io.ReadFull(os.Stdin, start[:]); err != nil {
		t.Fatal(err)
	}
	if mode == "hold" {
		return
	}
	for i := 0; i < 3; i++ {
		if err := recordFailureAt(path); err != nil {
			t.Fatal(err)
		}
		if err := recordSuccessAt(path); err != nil {
			t.Fatal(err)
		}
	}
}

type lockoutChild struct {
	cmd   *exec.Cmd
	input io.WriteCloser
	log   *bytes.Buffer
}

func startLockoutChild(t *testing.T, path, mode string) lockoutChild {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLockoutProcessHelper$")
	cmd.Env = append(os.Environ(), "CITADEL_LOCKOUT_TEST_MODE="+mode, "CITADEL_LOCKOUT_TEST_PATH="+path)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	log := new(bytes.Buffer)
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper did not reach its owned barrier: %q %v", line, err)
	}
	return lockoutChild{cmd, input, log}
}

func TestLockoutTransactionsAcrossActualProcesses(t *testing.T) {
	path := OpenLegacyPasscodeLockout(t.TempDir()).path
	const processes = 6
	children := make([]lockoutChild, processes)
	for i := range children {
		children[i] = startLockoutChild(t, path, "transactions")
	}
	// All processes exist before any transaction begins: no timing sleeps or
	// in-process mutex can supply the required cross-process serialization.
	for _, child := range children {
		if _, err := child.input.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		_ = child.input.Close()
	}
	for _, child := range children {
		if err := child.cmd.Wait(); err != nil {
			t.Fatalf("helper failed: %v %s", err, child.log.String())
		}
	}
	state := readLockoutAt(path, time.Now())
	if state.FailedAttempts != 3*processes || len(state.FailureTimes) != 3*processes ||
		state.LastVerifiedAt.IsZero() || state.LockoutUntil.Before(time.Now()) {
		t.Fatalf("lost failure/anchor across process transactions: %+v", state)
	}
	anchor := state.LastVerifiedAt
	if err := (&Lockout{path: path}).Reset(); err != nil {
		t.Fatal(err)
	}
	state = readLockoutAt(path, time.Now())
	if state.FailedAttempts != 0 || !state.LockoutUntil.IsZero() || !state.LastVerifiedAt.Equal(anchor) {
		t.Fatalf("reset did not preserve only the verification anchor: %+v", state)
	}
}

func TestLockoutKernelLockIsReleasedAfterProcessDeath(t *testing.T) {
	path := OpenLegacyPasscodeLockout(t.TempDir()).path
	if err := recordFailureAt(path); err != nil {
		t.Fatal(err)
	}
	child := startLockoutChild(t, path, "hold")
	file, err := openLockoutFile(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := lockLockoutFile(file, time.Now()); err == nil {
		unlockLockoutFile(file)
		t.Fatal("separate process did not hold an exclusive kernel lock")
	}
	if err := (&Lockout{path: path}).Reset(); !errors.Is(err, ErrLockoutUnavailable) {
		t.Fatalf("reset did not honor another process's transaction: %v", err)
	}
	if state := readLockoutAt(path, time.Now()); state.FailedAttempts != 1 {
		t.Fatal("refused reset modified the counter")
	}
	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.cmd.Wait(); err == nil {
		t.Fatal("helper unexpectedly exited successfully")
	}
	if err := withLockoutFile(path, func() error { return nil }); err != nil {
		t.Fatalf("dead process retained the kernel lock: %v", err)
	}
	if err := (&Lockout{path: path}).Reset(); err != nil {
		t.Fatalf("reset remained refused after holder death: %v", err)
	}
}

func TestLockoutOSLockNeverCoversLegacySecretCheck(t *testing.T) {
	l := OpenLegacyPasscodeLockout(t.TempDir())
	called := false
	err := l.Attempt(func() bool {
		called = true
		// A fresh descriptor cannot acquire the lock if Attempt holds the OS
		// lock across this callback (the bcrypt/KDF boundary).
		if err := withLockoutFile(l.path, func() error { return nil }); err != nil {
			t.Errorf("secret callback ran under an OS lock: %v", err)
		}
		return false
	})
	if !called || !errors.Is(err, ErrWrongPIN) {
		t.Fatalf("unexpected attempt outcome: called=%v err=%v", called, err)
	}
}

func TestLockoutSidecarSurvivesAtomicReplacement(t *testing.T) {
	l := OpenLegacyPasscodeLockout(t.TempDir())
	_ = l.Status()
	if _, err := os.Stat(l.path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("read-only status created a sidecar: %v", err)
	}
	if err := recordFailureAt(l.path); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(l.path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && before.Mode().Perm() != 0o600 {
		t.Fatal("lock sidecar is not owner-only")
	}
	if err := recordSuccessAt(l.path); err != nil {
		t.Fatal(err)
	}
	if err := l.Reset(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(l.path + ".lock")
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("state replacement/reset replaced or deleted the lock inode")
	}
}

func TestLockoutUnavailableCannotIssueSessionOrChangePIN(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	// Admission reads an absent state; the correct KDF can finish, but its
	// success cannot be persisted through this invalid lock pathname.
	if err := os.Mkdir(v.lockoutPath()+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	session, err := v.Unlock("654321")
	if session != nil || !errors.Is(err, ErrLockoutUnavailable) {
		t.Fatalf("lock failure granted a session: %v", err)
	}
	before, err := os.ReadFile(v.headerPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.ChangePIN("654321", "938472", DefaultPolicy(), true); !errors.Is(err, ErrLockoutUnavailable) {
		t.Fatalf("PIN rotation ignored unavailable accounting: %v", err)
	}
	after, err := os.ReadFile(v.headerPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused PIN rotation rewrote the vault")
	}
	l := OpenLegacyPasscodeLockout(t.TempDir())
	if err := os.Mkdir(l.path+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := l.Attempt(always(true)); !errors.Is(err, ErrLockoutUnavailable) {
		t.Fatalf("legacy gate ignored unavailable accounting: %v", err)
	}
}

func TestLockoutCorruptStateIsNotReplacedWithCleanCounter(t *testing.T) {
	l := OpenLegacyPasscodeLockout(t.TempDir())
	raw := []byte("failed_attempts: [\n")
	if err := os.WriteFile(l.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	err := l.Attempt(func() bool { called = true; return true })
	if called || !errors.Is(err, ErrLockoutUnavailable) {
		t.Fatalf("corrupt state admitted a secret check: %v", err)
	}
	for _, mutation := range []func() error{
		func() error { return recordFailureAt(l.path) },
		func() error { return recordSuccessAt(l.path) },
		l.Reset,
	} {
		if err := mutation(); !errors.Is(err, ErrLockoutUnavailable) {
			t.Fatalf("corrupt state accepted a transaction: %v", err)
		}
	}
	if got, err := os.ReadFile(l.path); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("refusal replaced the corrupt state")
	}
}

func TestLockoutRefusesLinkInsteadOfMutatingItsTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires a separately provisioned Windows privilege")
	}
	dir := t.TempDir()
	l := OpenLegacyPasscodeLockout(dir)
	target := filepath.Join(dir, "not-a-lock")
	raw := []byte("untouched")
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, l.path+".lock"); err != nil {
		t.Fatal(err)
	}
	if err := recordFailureAt(l.path); !errors.Is(err, ErrLockoutUnavailable) {
		t.Fatalf("symlink lock accepted: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("refused lock modified its target")
	}
}
