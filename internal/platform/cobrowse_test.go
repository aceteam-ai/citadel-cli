package platform

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// TestCobrowse_StopWhenNotRunning verifies Stop is a safe no-op with no session
// and that calling it twice does not panic. The shutdown hook in cmd/work.go
// relies on this being safe to call unconditionally on every worker exit.
func TestCobrowse_StopWhenNotRunning(t *testing.T) {
	m := &CobrowseManager{driver: DriverAI, debugPort: DefaultCobrowseDebugPort}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop with no session should not error: %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("second Stop should not error: %v", err)
	}
	if m.IsRunning() {
		t.Fatal("IsRunning should be false after Stop with no session")
	}
}

// TestCobrowse_ReaperDetectsExit verifies the load-bearing process-death fix:
// after the managed process exits on its own (simulating a Chromium crash),
// isRunningLocked flips to false instead of reporting the dead process as alive.
// Uses `sleep` as a stand-in process so the test needs no Chromium or display.
func TestCobrowse_ReaperDetectsExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sleep as a stand-in process")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	m := &CobrowseManager{driver: DriverAI, debugPort: DefaultCobrowseDebugPort}

	// Mirror the lifecycle Start() sets up: a running process plus the reaper
	// goroutine that closes exited when the process terminates.
	cmd := exec.Command("sleep", "0.2")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in process: %v", err)
	}
	exited := make(chan struct{})
	m.cmd = cmd
	m.exited = exited
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	if !m.IsRunning() {
		t.Fatal("expected IsRunning=true while stand-in process is alive")
	}

	// Wait for the process to exit on its own; the reaper should close exited.
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("reaper did not observe process exit")
	}

	if m.IsRunning() {
		t.Fatal("expected IsRunning=false after the process exited (crash detection)")
	}
}

// TestCobrowse_StopKillsRunningProcess verifies Stop kills a live process,
// resets the driver to AI, and is safe to call again afterwards.
func TestCobrowse_StopKillsRunningProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sleep as a stand-in process")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	m := &CobrowseManager{driver: DriverHuman, debugPort: DefaultCobrowseDebugPort}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in process: %v", err)
	}
	exited := make(chan struct{})
	m.cmd = cmd
	m.exited = exited
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	if !m.IsRunning() {
		t.Fatal("expected IsRunning=true before Stop")
	}
	_ = m.Stop()
	if m.IsRunning() {
		t.Fatal("expected IsRunning=false after Stop")
	}
	if m.driver != DriverAI {
		t.Fatalf("expected driver reset to ai after Stop, got %q", m.driver)
	}
	// Second Stop on the now-cleared manager must remain a safe no-op.
	if err := m.Stop(); err != nil {
		t.Fatalf("second Stop should not error: %v", err)
	}
}
