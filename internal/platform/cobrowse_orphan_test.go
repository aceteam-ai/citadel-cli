// internal/platform/cobrowse_orphan_test.go
//
// Tests for the co-browse orphan / stale-:9222 reclaim logic (issue #396). The
// port lookup and PID kill are behind package-var seams (portOwnerLookup,
// pidKiller), so these tests exercise the reclaim decision tree deterministically
// without binding the real CDP port or launching Chromium.
package platform

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// withStubbedPortSeams swaps portOwnerLookup / pidKiller for the duration of a
// test and restores them via t.Cleanup, so tests never touch real processes.
func withStubbedPortSeams(t *testing.T, lookup func(int) (int, bool), kill func(int) error) {
	t.Helper()
	origLookup, origKill := portOwnerLookup, pidKiller
	portOwnerLookup = lookup
	pidKiller = kill
	t.Cleanup(func() {
		portOwnerLookup = origLookup
		pidKiller = origKill
	})
}

func TestReclaimStalePort_NoOwner(t *testing.T) {
	killed := false
	withStubbedPortSeams(t,
		func(int) (int, bool) { return 0, false },
		func(int) error { killed = true; return nil },
	)
	reclaimed, err := reclaimStalePort(9222)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reclaimed {
		t.Fatal("expected reclaimed=false when no owner holds the port")
	}
	if killed {
		t.Fatal("kill must not run when the port is free")
	}
}

func TestReclaimStalePort_KillsOwner(t *testing.T) {
	var killedPID int
	withStubbedPortSeams(t,
		func(int) (int, bool) { return 4242, true },
		func(pid int) error { killedPID = pid; return nil },
	)
	reclaimed, err := reclaimStalePort(9222)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reclaimed {
		t.Fatal("expected reclaimed=true when a stale owner was killed")
	}
	if killedPID != 4242 {
		t.Fatalf("expected to kill pid 4242, killed %d", killedPID)
	}
}

func TestReclaimStalePort_KillError(t *testing.T) {
	withStubbedPortSeams(t,
		func(int) (int, bool) { return 4242, true },
		func(int) error { return errors.New("boom") },
	)
	reclaimed, err := reclaimStalePort(9222)
	if err == nil {
		t.Fatal("expected an error when the port owner cannot be killed")
	}
	if reclaimed {
		t.Fatal("reclaimed must be false when the kill failed")
	}
}

// TestReclaimStalePort_NeverKillsSelf guards the defensive check: if the lookup
// ever returns this process's own PID, reclaim must NOT kill it.
func TestReclaimStalePort_NeverKillsSelf(t *testing.T) {
	killed := false
	withStubbedPortSeams(t,
		func(int) (int, bool) { return os.Getpid(), true },
		func(int) error { killed = true; return nil },
	)
	reclaimed, err := reclaimStalePort(9222)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reclaimed || killed {
		t.Fatal("reclaim must never kill the current process")
	}
}

func TestFirstPID(t *testing.T) {
	if pid, ok := firstPID("12345\n"); !ok || pid != 12345 {
		t.Fatalf("firstPID single = (%d,%v), want (12345,true)", pid, ok)
	}
	if pid, ok := firstPID("999\n1000\n"); !ok || pid != 999 {
		t.Fatalf("firstPID multi = (%d,%v), want (999,true)", pid, ok)
	}
	if _, ok := firstPID("\n\n"); ok {
		t.Fatal("firstPID on blank output should report not-found")
	}
	if _, ok := firstPID("notanumber"); ok {
		t.Fatal("firstPID on junk should report not-found")
	}
}

func TestPidFromSS(t *testing.T) {
	// Representative `ss -ltnp` line for a listener with a process column.
	line := `LISTEN 0 128 127.0.0.1:9222 0.0.0.0:* users:(("chrome",pid=54321,fd=88))`
	if pid, ok := pidFromSS(line); !ok || pid != 54321 {
		t.Fatalf("pidFromSS = (%d,%v), want (54321,true)", pid, ok)
	}
	if _, ok := pidFromSS("LISTEN 0 128 127.0.0.1:9222 0.0.0.0:*"); ok {
		t.Fatal("pidFromSS without a pid= token should report not-found")
	}
}

func TestIsProcessGoneErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, true},
		{"process-done", os.ErrProcessDone, true},
		{"already-finished", errors.New("os: process already finished"), true},
		{"no-such-process", errors.New("no such process"), true},
		{"other", errors.New("permission denied"), false},
	}
	for _, c := range cases {
		if got := isProcessGoneErr(c.err); got != c.want {
			t.Errorf("isProcessGoneErr(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestDefaultKillPID_AlreadyDead verifies the real kill path treats an
// already-exited PID as success (goal state reached), using a short-lived
// stand-in process so no Chromium/port is needed.
func TestDefaultKillPID_AlreadyDead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX true as a stand-in process")
	}
	bin, err := exec.LookPath("true")
	if err != nil {
		t.Skip("true not available")
	}
	cmd := exec.Command(bin)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait() // let it exit so the PID is already gone
	if err := defaultKillPID(pid); err != nil {
		t.Fatalf("defaultKillPID on an already-dead PID should succeed, got %v", err)
	}
}

// TestDefaultKillPID_KillsLiveProcess verifies defaultKillPID actually kills a
// live child and waits for it to disappear.
func TestDefaultKillPID_KillsLiveProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sleep as a stand-in process")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in: %v", err)
	}
	pid := cmd.Process.Pid
	// Reap it in the background so killing it leaves no zombie for the probe.
	go func() { _ = cmd.Wait() }()
	if !pidAlive(pid) {
		t.Fatal("expected stand-in process to be alive before kill")
	}
	if err := defaultKillPID(pid); err != nil {
		t.Fatalf("defaultKillPID on a live process should succeed, got %v", err)
	}
	if pidAlive(pid) {
		t.Fatal("expected process to be gone after defaultKillPID")
	}
}
