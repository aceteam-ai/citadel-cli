package worklock

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestIsHeldNoFile verifies the read-only detector reports not-held when no lock
// file exists (a node with no worker ever started).
func TestIsHeldNoFile(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")
	held, pid := IsHeld(stateDir)
	if held {
		t.Fatalf("IsHeld on a missing lock file = held (pid %d), want not held", pid)
	}
}

// TestIsHeldStaleReusedPID verifies that a lock FILE naming a live-but-non-citadel
// PID (a PID reused by an unrelated program after a reboot) reports not-held: only
// a live citadel holder counts. Mirrors Acquire's reclaim classification so a stale
// record never wedges the control center into monitor-only mode. Linux-only because
// the reused-PID discrimination relies on /proc/<pid>/cmdline.
func TestIsHeldStaleReusedPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reused-PID cmdline detection is Linux-only (/proc)")
	}
	stateDir := filepath.Join(t.TempDir(), "network")
	path := LockPathForStateDir(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// A live, non-citadel process stands in for a reused PID. It holds no OS lock,
	// so the flock probe would succeed; but even seeded as the recorded holder the
	// detector must report not-held because its cmdline is not a citadel process.
	proc := exec.Command("sleep", "30")
	if err := proc.Start(); err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	defer func() { _ = proc.Process.Kill(); _, _ = proc.Process.Wait() }()

	stale := lockRecord{PID: proc.Process.Pid, StartUnix: 1, Version: "vOLD"}
	if err := os.WriteFile(path, encodeRecord(stale), 0o600); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}

	held, pid := IsHeld(stateDir)
	if held {
		t.Fatalf("IsHeld on a stale reused-PID lock = held (pid %d), want not held", pid)
	}
}

// TestIsHeldDeadHolder verifies a lock file whose recorded PID is dead (and no OS
// lock is held) reports not-held.
func TestIsHeldDeadHolder(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")
	path := LockPathForStateDir(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// PID 2^30 is far above any real PID on these platforms: guaranteed dead.
	dead := lockRecord{PID: 1 << 30, StartUnix: 1, Version: "vOLD"}
	if err := os.WriteFile(path, encodeRecord(dead), 0o600); err != nil {
		t.Fatalf("seed dead lock: %v", err)
	}
	held, pid := IsHeld(stateDir)
	if held {
		t.Fatalf("IsHeld on a dead-holder lock = held (pid %d), want not held", pid)
	}
}

// TestIsHeldLiveWorker verifies the positive path: while a live citadel process
// holds the single-instance lock, IsHeld reports held with that holder's PID, and
// the read-only probe does NOT disturb the holder (it can be probed repeatedly and
// the holder still owns the lock afterward). The holder is a subprocess of this
// test binary whose argv contains the literal "citadel" so processIsCitadel's
// /proc cmdline check classifies it correctly. Linux-only for that reason.
func TestIsHeldLiveWorker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("live-holder detection uses /proc cmdline classification (Linux-only)")
	}
	stateDir := filepath.Join(t.TempDir(), "network")

	// Spawn the helper below, which acquires the lock and blocks. The "citadel"
	// argument makes its /proc cmdline match processIsCitadel.
	helper := exec.Command(os.Args[0], "-test.run=TestLockHolderHelper", "--", "citadel-lock-holder")
	helper.Env = append(os.Environ(),
		"WORKLOCK_HELPER=1",
		"WORKLOCK_HELPER_STATEDIR="+stateDir,
	)
	// The helper keeps itself alive by blocking on stdin (see TestLockHolderHelper
	// for why: a bare `select {}` gets the helper killed by Go's runtime deadlock
	// detector — issue #538). Hold the write end open for the whole test so the
	// helper stays alive until we kill it.
	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	defer func() { _ = stdin.Close() }()
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	helper.Stderr = &stderr
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() { _ = helper.Process.Kill(); _, _ = helper.Process.Wait() }()

	// Wait for the helper to signal it has acquired the lock. The signal must be
	// the literal "ready" line: anything else (EOF from a crashed helper, testing
	// framework output) means the helper is NOT holding the lock, and treating it
	// as ready would surface later as a baffling "not held" failure.
	type readResult struct {
		line string
		err  error
	}
	readCh := make(chan readResult, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		readCh <- readResult{line, err}
	}()
	select {
	case r := <-readCh:
		if r.err != nil || strings.TrimSpace(r.line) != "ready" {
			t.Fatalf("helper did not signal readiness: read %q (err %v); helper stderr:\n%s", r.line, r.err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("helper did not acquire the lock within 10s; helper stderr:\n%s", stderr.String())
	}

	held, pid := IsHeld(stateDir)
	if !held {
		t.Fatalf("IsHeld while a live citadel worker holds the lock = not held, want held; helper stderr:\n%s", stderr.String())
	}
	if pid != helper.Process.Pid {
		t.Errorf("IsHeld holder PID = %d, want helper PID %d", pid, helper.Process.Pid)
	}

	// Probing must not steal the lock: a second Acquire from THIS process must still
	// be refused because the helper still holds it.
	if l, err := Acquire(stateDir, "", nil); err == nil {
		l.Release()
		t.Fatal("Acquire succeeded after IsHeld probe; the read-only probe must not release the holder's lock")
	}

	// A repeat probe still reports held (idempotent, non-disruptive).
	if held2, _ := IsHeld(stateDir); !held2 {
		t.Fatal("second IsHeld probe = not held; the first probe disturbed the holder")
	}
}

// TestLockHolderHelper is not a real test: it is the subprocess entrypoint spawned
// by TestIsHeldLiveWorker. Gated by WORKLOCK_HELPER so it is inert during a normal
// test run. It acquires the worker lock, self-verifies IsHeld sees the lock, prints
// a readiness line, and blocks on stdin so the parent can observe a live holder.
//
// The keep-alive MUST block in a syscall (reading stdin), not in the Go scheduler.
// An earlier version used `select {}`, which blocked every goroutine in the helper
// and tripped Go's runtime deadlock detector ("all goroutines are asleep"), killing
// the helper (and releasing the flock) milliseconds after "ready" was written. The
// parent usually won the race to IsHeld, but on a loaded machine it was descheduled
// first and observed a genuinely dead holder — the flake in issue #538. A goroutine
// blocked reading a pipe is invisible to the deadlock detector, and stdin EOF also
// gives the helper a clean exit if the parent dies without killing it.
func TestLockHolderHelper(t *testing.T) {
	if os.Getenv("WORKLOCK_HELPER") != "1" {
		return
	}
	stateDir := os.Getenv("WORKLOCK_HELPER_STATEDIR")
	l, err := Acquire(stateDir, "vHELPER", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: Acquire failed: %v\n", err)
		os.Exit(2)
	}
	defer l.Release()
	// Self-verify before signaling: "ready" must mean "a probe from any process
	// would observe the lock held by this PID". The flock and the on-disk record
	// are kernel/filesystem state, so if this process sees them, the parent will.
	if held, pid := IsHeld(stateDir); !held || pid != os.Getpid() {
		fmt.Fprintf(os.Stderr, "helper: self-check IsHeld = (%v, %d), want (true, %d)\n", held, pid, os.Getpid())
		os.Exit(2)
	}
	// Signal readiness to the parent, then block until stdin closes (parent exit)
	// or we are killed.
	_, _ = os.Stdout.WriteString("ready\n")
	_ = os.Stdout.Sync()
	_, _ = io.Copy(io.Discard, os.Stdin)
}
