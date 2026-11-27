//go:build darwin

package platform

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDarwinProcessStartTokenAndOwnerCheck(t *testing.T) {
	tok, err := processStartToken(os.Getpid())
	if err != nil || tok <= 0 {
		t.Fatalf("own start token: %d, %v", tok, err)
	}
	if !sessionOwnerAlive(os.Getpid(), tok) {
		t.Error("this process with its own start token must be alive")
	}
	if sessionOwnerAlive(os.Getpid(), tok+1) {
		t.Error("a different start time must read as a reused PID")
	}
	if sessionOwnerAlive(0, 0) {
		t.Error("pid 0 is never an owner")
	}
}

func TestDarwinProcessCommandLine(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	got, err := processCommandLine(cmd.Process.Pid)
	if err != nil || !strings.Contains(got, "sleep 30") {
		t.Fatalf("command line: %q, %v", got, err)
	}
}

// A group kill must take down the whole tree, including a grandchild.
func TestDarwinKillProcessGroupKillsTree(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 60 & echo $!; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, _ := out.Read(buf)
	child, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatalf("grandchild pid: %v", err)
	}
	if pg, err := syscall.Getpgid(child); err != nil || pg != cmd.Process.Pid {
		t.Fatalf("grandchild group %d, %v; want %d", pg, err, cmd.Process.Pid)
	}
	if err := killProcessGroup(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(child, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if syscall.Kill(child, 0) == nil {
		t.Error("grandchild survived the group kill")
	}
	if err := killProcessGroup(cmd.Process.Pid); err != nil {
		t.Errorf("an already-gone group is success, got %v", err)
	}
	if err := killProcessGroup(1); err == nil {
		t.Error("must refuse group 1")
	}
}

func TestDarwinRunnerSelected(t *testing.T) {
	if CobrowseRunnerName() != "headless" {
		t.Errorf("runner %q", CobrowseRunnerName())
	}
}
