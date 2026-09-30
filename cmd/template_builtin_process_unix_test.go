//go:build !windows

package cmd

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
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
	processTree := configurePapercraftProcessTree(&renderCommand{cmd: cmd})
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("Chromium must launch in a dedicated process group")
	}
	if cmd.Cancel == nil {
		t.Fatal("Chromium process group cancellation is not configured")
	}
	if processTree == nil {
		t.Fatal("Chromium process group explicit termination is not configured")
	}
}

func TestChromiumShutdownKillsHelpersAfterRootExited(t *testing.T) {
	originalCommand := renderLimitedCommand
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	renderLimitedCommand = func(ctx context.Context, _ []string, binary string, args ...string) (*renderCommand, error) {
		return &renderCommand{cmd: exec.CommandContext(ctx, binary, args...), stopScope: func() error {
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

func TestPostWaitCleanupNeverSignalsReusedProcessGroup(t *testing.T) {
	root := exec.CommandContext(context.Background(), "/bin/sh", "-c", "exit 0")
	scopeStops := 0
	limited := &renderCommand{cmd: root, stopScope: func() error {
		scopeStops++
		return nil
	}}
	processTree := configurePapercraftProcessTree(limited)
	if err := root.Start(); err != nil {
		t.Fatal(err)
	}
	if err := root.Wait(); err != nil {
		t.Fatal(err)
	}

	// Deterministically model the kernel reusing the reaped root's numeric
	// PID/PGID for an unrelated process group. Post-Wait cleanup must use only
	// the unique scope identity and must not signal this replacement group.
	unrelated := exec.Command("/bin/sh", "-c", "exec sleep 60")
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unrelated.Process.Kill()
		_ = unrelated.Wait()
	})
	root.Process = unrelated.Process

	if err := processTree.cleanupAfterWait(); err != nil {
		t.Fatalf("post-Wait cleanup: %v", err)
	}
	if scopeStops != 1 {
		t.Fatalf("scope stops = %d, want 1", scopeStops)
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("post-Wait cleanup signaled reused process group: %v", err)
	}
}

func TestCommandCancellationKillsLiveProcessGroupBeforeScopeCleanup(t *testing.T) {
	var scopeStopped bool
	limited := &renderCommand{
		cmd: exec.CommandContext(context.Background(), "/bin/sh", "-c", "exec sleep 60"),
		stopScope: func() error {
			scopeStopped = true
			return nil
		},
	}
	_ = configurePapercraftProcessTree(limited)
	if err := limited.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := limited.cmd.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	_ = limited.cmd.Wait()
	if !scopeStopped {
		t.Fatal("live cancellation did not stop the systemd scope")
	}
}

func TestChromiumShutdownKillsLiveChildWhenScopeCleanupFails(t *testing.T) {
	originalCommand := renderLimitedCommand
	want := errors.New("systemctl stop denied")
	renderLimitedCommand = func(ctx context.Context, _ []string, binary string, args ...string) (*renderCommand, error) {
		return &renderCommand{
			cmd:       exec.CommandContext(ctx, binary, args...),
			stopScope: func() error { return want },
		}, nil
	}
	t.Cleanup(func() { renderLimitedCommand = originalCommand })

	process, err := startChromiumProcess(context.Background(), "/bin/sh", []string{"-c", "exec sleep 60"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = process.Shutdown()
	if !errors.Is(err, want) {
		t.Fatalf("Shutdown error = %v, want scope cleanup failure", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Shutdown took %s after scope cleanup failure", elapsed)
	}
	if err := process.processTree.cmd.Process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("live Chromium child survived failed scope cleanup: %v", err)
	}
}

func TestChromiumObserverErrorReturnsBeforeShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	limited := &renderCommand{
		cmd:       exec.CommandContext(ctx, "/bin/sh", "-c", "exec sleep 60"),
		stopScope: func() error { return nil },
	}
	processTree := configurePapercraftProcessTree(limited)
	if err := limited.cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}

	want := errors.New("waitid refused")
	process := &execChromiumProcess{
		cancel:      cancel,
		cmd:         limited.cmd,
		processTree: processTree,
		done:        make(chan struct{}),
		observeErr:  want,
		waitDone:    make(chan struct{}),
	}
	close(process.done)
	t.Cleanup(func() { _ = process.Shutdown() })

	errCh := make(chan error, 1)
	go func() { errCh <- process.WaitErr() }()
	select {
	case err := <-errCh:
		if !errors.Is(err, want) {
			t.Fatalf("WaitErr = %v, want observer failure", err)
		}
	case <-time.After(2 * time.Second):
		_ = process.Shutdown()
		t.Fatal("WaitErr blocked on cmd.Wait after observer failure")
	}

	started := time.Now()
	if err := process.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Shutdown took %s after observer failure", elapsed)
	}
	if err := limited.cmd.Process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Chromium child survived observer-error cleanup: %v", err)
	}
}

func TestFFmpegAbortKillsLiveChildWhenScopeCleanupFails(t *testing.T) {
	binDir := t.TempDir()
	ffmpeg := filepath.Join(binDir, "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	originalCommand := renderLimitedCommand
	want := errors.New("systemctl stop denied")
	var child *exec.Cmd
	renderLimitedCommand = func(ctx context.Context, _ []string, binary string, args ...string) (*renderCommand, error) {
		child = exec.CommandContext(ctx, binary, args...)
		return &renderCommand{cmd: child, stopScope: func() error { return want }}, nil
	}
	t.Cleanup(func() { renderLimitedCommand = originalCommand })

	encoder, err := startPapercraftFFmpeg(context.Background(), papercraftEncoderConfig{
		Width: 16, Height: 16, FPS: 1, DurationSeconds: 1, OutputPath: filepath.Join(t.TempDir(), "out.mp4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = encoder.Abort()
	if !errors.Is(err, want) {
		t.Fatalf("Abort error = %v, want scope cleanup failure", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Abort took %s after scope cleanup failure", elapsed)
	}
	if err := child.Process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("live ffmpeg child survived failed scope cleanup: %v", err)
	}
}

func TestRealFFmpegLeavesOnlyReportedOutput(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is unavailable")
	}

	originalCommand := renderLimitedCommand
	renderLimitedCommand = func(ctx context.Context, env []string, binary string, args ...string) (*renderCommand, error) {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = append([]string(nil), env...)
		return &renderCommand{cmd: cmd, stopScope: func() error { return nil }}, nil
	}
	t.Cleanup(func() { renderLimitedCommand = originalCommand })

	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "render.mp4")
	encoder, err := startPapercraftFFmpeg(context.Background(), papercraftEncoderConfig{
		Width: 16, Height: 16, FPS: 1, DurationSeconds: 1, OutputPath: outPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	frame := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			frame.Set(x, y, color.RGBA{R: 0x44, G: 0x88, B: 0xcc, A: 0xff})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		t.Fatal(err)
	}
	if err := encoder.WriteFrame(encoded.Bytes()); err != nil {
		_ = encoder.Abort()
		t.Fatal(err)
	}
	if err := encoder.Finish(); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := validateTemplateOutputTree(root, []string{"render.mp4"}, templateMaxOutputFiles, templateMaxOutputEntries, templateMaxOutputBytes); err != nil {
		t.Fatalf("real ffmpeg output violates template output contract: %v", err)
	}
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
		cmd: exec.CommandContext(context.Background(), "/bin/sh", "-c", "exit 0"),
		stopScope: func() error {
			return want
		},
	}
	processTree := configurePapercraftProcessTree(limited)
	if err := limited.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := limited.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	err := processTree.cleanupAfterWait()
	if !errors.Is(err, want) {
		t.Fatalf("terminate error = %v, want scope cleanup failure", err)
	}
}
