//go:build !windows

package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestConfigurePapercraftProcessTreeUnix(t *testing.T) {
	cmd := exec.Command("unused")
	terminate := configurePapercraftProcessTree(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("Chromium must launch in a dedicated process group")
	}
	if cmd.Cancel == nil {
		t.Fatal("Chromium process group cancellation is not configured")
	}
	if terminate == nil {
		t.Fatal("Chromium process group explicit termination is not configured")
	}
}

func TestChromiumShutdownKillsHelpersAfterRootExited(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	process, err := startChromiumProcess(
		context.Background(),
		"/bin/sh",
		[]string{"-c", "sleep 60 </dev/null >/dev/null 2>&1 & echo $! > \"$1\"", "sh", pidFile},
		os.Environ(),
	)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-process.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("root process did not exit")
	}
	rawPID, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("helper was not alive before Shutdown: %v", err)
	}

	if err := process.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("helper process %d survived Shutdown after its root exited", childPID)
}
