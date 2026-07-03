package worklock

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDecideStaleLock(t *testing.T) {
	const self = 4242
	tests := []struct {
		name        string
		holderPID   int
		selfPID     int
		holderLive  bool
		holderIsCit bool
		want        staleLockAction
	}{
		{"empty lock file (pid 0) reclaims", 0, self, false, false, actionReclaim},
		{"negative pid reclaims", -1, self, false, false, actionReclaim},
		{"own pid recorded reclaims", self, self, true, true, actionReclaim},
		{"dead holder reclaims", 1234, self, false, false, actionReclaim},
		{"live citadel holder refuses", 1234, self, true, true, actionRefuse},
		{"live NON-citadel holder (reused PID) reclaims", 1234, self, true, false, actionReclaim},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideStaleLock(tt.holderPID, tt.selfPID, tt.holderLive, tt.holderIsCit)
			if got != tt.want {
				t.Fatalf("decideStaleLock(%d,%d,live=%v,cit=%v) = %v, want %v",
					tt.holderPID, tt.selfPID, tt.holderLive, tt.holderIsCit, got, tt.want)
			}
		})
	}
}

// TestRecordRoundTrip verifies the lock record encodes to JSON and decodes back,
// and that a legacy bare-PID lock file still parses (so an in-place upgrade never
// misreads a file written by an older worker).
func TestRecordRoundTrip(t *testing.T) {
	in := lockRecord{PID: 4321, StartUnix: 1700000000, Version: "v2.65.0"}
	out := decodeRecord(encodeRecord(in))
	if out != in {
		t.Fatalf("round-trip record = %+v, want %+v", out, in)
	}

	// Legacy bare-PID line.
	legacy := decodeRecord([]byte("9876\n"))
	if legacy.PID != 9876 || legacy.StartUnix != 0 || legacy.Version != "" {
		t.Fatalf("legacy decode = %+v, want PID=9876 with no start/version", legacy)
	}

	// Empty / garbage yields a zero record (never a false live-holder).
	if got := decodeRecord([]byte("   \n")); got.PID != 0 {
		t.Fatalf("empty decode PID = %d, want 0", got.PID)
	}
	if got := decodeRecord([]byte("not-a-pid")); got.PID != 0 {
		t.Fatalf("garbage decode PID = %d, want 0", got.PID)
	}
}

// TestAcquireStampsRecord verifies a fresh acquire records this process's PID, a
// start timestamp, and the supplied version into the lock file.
func TestAcquireStampsRecord(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")
	l, err := Acquire(stateDir, "v9.9.9", nil)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	defer l.Release()

	data, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	rec := decodeRecord(data)
	if rec.PID != os.Getpid() {
		t.Errorf("record PID = %d, want %d", rec.PID, os.Getpid())
	}
	if rec.StartUnix <= 0 {
		t.Errorf("record StartUnix = %d, want > 0", rec.StartUnix)
	}
	if rec.Version != "v9.9.9" {
		t.Errorf("record Version = %q, want %q", rec.Version, "v9.9.9")
	}
}

// TestReusedPIDReclaimed simulates the reused-PID wedge: a stale lock file records
// a PID that is alive but belongs to an unrelated (non-citadel) process. Because
// no live citadel worker holds the OS lock, Acquire must reclaim it rather than
// refuse forever. On Linux we spawn a real short-lived non-citadel process and use
// its PID as the recorded holder; the /proc cmdline check then classifies it as a
// reused PID. On non-Linux (no /proc), processIsCitadel is a no-op returning true,
// so this test is Linux-only.
func TestReusedPIDReclaimed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reused-PID cmdline detection is Linux-only (/proc)")
	}
	stateDir := filepath.Join(t.TempDir(), "network")
	path := LockPathForStateDir(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Spawn a live, non-citadel process to stand in for a PID that was reused by an
	// unrelated program after a reboot. It sleeps long enough to stay alive for the
	// duration of this test.
	proc := exec.Command("sleep", "30")
	if err := proc.Start(); err != nil {
		t.Fatalf("spawn helper process: %v", err)
	}
	defer func() { _ = proc.Process.Kill(); _, _ = proc.Process.Wait() }()

	// Seed a stale record naming the live-but-non-citadel PID. No process holds the
	// OS lock, so Acquire should reclaim (alive but cmdline != citadel).
	stale := lockRecord{PID: proc.Process.Pid, StartUnix: 1, Version: "vOLD"}
	if err := os.WriteFile(path, encodeRecord(stale), 0o600); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}

	reclaimed := false
	l, err := Acquire(stateDir, "vNEW", func(string, ...any) { reclaimed = true })
	if err != nil {
		t.Fatalf("Acquire should reclaim a stale reused-PID lock, got error: %v", err)
	}
	defer l.Release()
	if !reclaimed {
		t.Errorf("expected a reclaim log for the reused non-citadel PID, got none")
	}
}

// TestErrAlreadyRunningMessage pins the exact operator-facing refusal wording.
func TestErrAlreadyRunningMessage(t *testing.T) {
	e := &ErrAlreadyRunning{PID: 4242, StartTime: time.Unix(1700000000, 0), Path: "/x/citadel-work.lock"}
	got := e.Error()
	if !strings.Contains(got, "citadel worker already running (PID 4242, started ") {
		t.Errorf("message = %q, missing expected prefix", got)
	}
	if !strings.Contains(got, "refusing to start a second instance") {
		t.Errorf("message = %q, missing refusal clause", got)
	}
}

func TestLockPathForStateDir(t *testing.T) {
	// The lock must live in the PARENT of the network state dir so ClearState
	// (which RemoveAll's the network dir) never deletes it.
	got := LockPathForStateDir("/home/u/citadel-node/network")
	want := filepath.FromSlash("/home/u/citadel-node/" + LockFileName)
	if got != want {
		t.Fatalf("LockPathForStateDir = %q, want %q", got, want)
	}

	// Trailing slash must not change the result.
	got2 := LockPathForStateDir("/home/u/citadel-node/network/")
	if got2 != want {
		t.Fatalf("LockPathForStateDir(trailing slash) = %q, want %q", got2, want)
	}
}

// TestAcquireSecondIsRejected verifies the core guarantee: while one holder has
// the lock, a second Acquire on the same node is refused with *ErrAlreadyRunning
// carrying the holder PID. A third attempt succeeds once the first releases.
func TestAcquireSecondIsRejected(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")

	first, err := Acquire(stateDir, "", nil)
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}
	if first == nil {
		t.Fatal("first Acquire returned nil lock")
	}

	// Second acquire while first is held: must be refused.
	second, err := Acquire(stateDir, "", nil)
	if err == nil {
		second.Release()
		t.Fatal("second Acquire succeeded while first held the lock; expected refusal")
	}
	are, ok := err.(*ErrAlreadyRunning)
	if !ok {
		t.Fatalf("second Acquire error = %T (%v), want *ErrAlreadyRunning", err, err)
	}
	if are.PID <= 0 {
		t.Errorf("ErrAlreadyRunning.PID = %d, want the holder PID (>0)", are.PID)
	}

	// Release the first and confirm a new Acquire now succeeds (lock reusable).
	first.Release()
	third, err := Acquire(stateDir, "", nil)
	if err != nil {
		t.Fatalf("Acquire after Release failed: %v", err)
	}
	third.Release()
}

// TestReleaseIdempotent verifies Release can be called multiple times safely.
func TestReleaseIdempotent(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")
	l, err := Acquire(stateDir, "", nil)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	l.Release()
	l.Release() // must not panic
}

// TestStaleLockReclaimed simulates a dead prior holder: a lock file exists with a
// PID that is not alive, and no live process holds the OS lock. Acquire must
// reclaim it (the flock/LockFileEx is free once the prior process exited).
func TestStaleLockReclaimed(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")

	// Hold and release to leave a lock file behind with a now-defunct PID record.
	first, err := Acquire(stateDir, "", nil)
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}
	// Simulate a crash where the file is NOT removed but the lock is freed on exit
	// by closing the fd without Remove. We cannot easily fork here, so we rely on
	// Release freeing the OS lock; the reclaim path is exercised by decideStaleLock
	// unit tests. This test asserts re-acquire works after the OS lock is free.
	first.Release()

	got, err := Acquire(stateDir, "", func(string, ...any) {})
	if err != nil {
		t.Fatalf("re-Acquire after prior holder exited failed: %v", err)
	}
	got.Release()
}
