package worklock

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() { _ = helper.Process.Kill(); _, _ = helper.Process.Wait() }()

	// Wait for the helper to signal it has acquired the lock.
	buf := make([]byte, 16)
	waitReady := make(chan struct{})
	go func() {
		_, _ = stdout.Read(buf)
		close(waitReady)
	}()
	select {
	case <-waitReady:
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not acquire the lock within 10s")
	}

	held, pid := IsHeld(stateDir)
	if !held {
		t.Fatalf("IsHeld while a live citadel worker holds the lock = not held, want held")
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
// test run. It acquires the worker lock, prints a readiness byte, and blocks so the
// parent can observe a live holder.
func TestLockHolderHelper(t *testing.T) {
	if os.Getenv("WORKLOCK_HELPER") != "1" {
		return
	}
	stateDir := os.Getenv("WORKLOCK_HELPER_STATEDIR")
	l, err := Acquire(stateDir, "vHELPER", nil)
	if err != nil {
		// Signal failure by exiting non-zero.
		os.Exit(2)
	}
	defer l.Release()
	// Signal readiness to the parent, then block until killed.
	_, _ = os.Stdout.WriteString("ready\n")
	_ = os.Stdout.Sync()
	select {}
}
