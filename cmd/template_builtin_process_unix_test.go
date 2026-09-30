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
	terminate := configurePapercraftProcessTree(&renderCommand{cmd: cmd})
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
	originalCommand := renderLimitedCommand
	renderLimitedCommand = func(ctx context.Context, _ []string, binary string, args ...string) (*renderCommand, error) {
		return &renderCommand{cmd: exec.CommandContext(ctx, binary, args...)}, nil
	}
	t.Cleanup(func() { renderLimitedCommand = originalCommand })

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
	var rawPID []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rawPID, err = os.ReadFile(pidFile)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("helper did not publish pid: %v", err)
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
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("helper process %d survived Shutdown after its root exited", childPID)
}

func TestChromiumShutdownKillsSetsidDescendantThroughScope(t *testing.T) {
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid is unavailable")
	}
	originalCommand := renderLimitedCommand
	pidFile := filepath.Join(t.TempDir(), "setsid-child.pid")
	var scopeStopCalled bool
	renderLimitedCommand = func(ctx context.Context, _ []string, binary string, args ...string) (*renderCommand, error) {
		cmd := exec.CommandContext(ctx, binary, args...)
		return &renderCommand{cmd: cmd, stopScope: func() error {
			scopeStopCalled = true
			rawPID, err := os.ReadFile(pidFile)
			if err != nil {
				return err
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
			if err != nil {
				return err
			}
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
			return nil
		}}, nil
	}
	t.Cleanup(func() { renderLimitedCommand = originalCommand })

	process, err := startChromiumProcess(
		context.Background(),
		"/bin/sh",
		[]string{"-c", `"$1" sh -c 'echo $$ > "$1"; exec sleep 60' sh "$2" </dev/null >/dev/null 2>&1 &`, "sh", setsid, pidFile},
		minimalRenderChildEnv(""),
	)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("root process did not exit")
	}
	var rawPID []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rawPID, err = os.ReadFile(pidFile)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("setsid child did not publish pid: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	childPGID, err := syscall.Getpgid(childPID)
	if err != nil {
		t.Fatal(err)
	}
	if childPGID != childPID {
		t.Fatal("test descendant did not escape through setsid")
	}
	if err := process.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !scopeStopCalled {
		t.Fatal("systemd scope cleanup was not invoked")
	}
}

func TestConfigurePapercraftProcessTreeReportsScopeCleanupFailure(t *testing.T) {
	want := errors.New("systemctl stop denied")
	limited := &renderCommand{
		cmd: exec.CommandContext(context.Background(), "/bin/sh", "-c", "exec sleep 60"),
		stopScope: func() error {
			return want
		},
	}
	terminate := configurePapercraftProcessTree(limited)
	if err := limited.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	err := terminate()
	_ = limited.cmd.Wait()
	if !errors.Is(err, want) {
		t.Fatalf("terminate error = %v, want scope cleanup failure", err)
	}
}
