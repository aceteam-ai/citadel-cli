//go:build windows

package platform

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsSuspendJobChild(t *testing.T) {
	if os.Getenv("CITADEL_TEST_SUSPEND_JOB_CHILD") != "1" {
		return
	}
	// Only the private test parent invokes this helper. Kill-on-close limits
	// its lifetime even when the proof fails; it never opens a browser/client.
	time.Sleep(time.Minute)
}

func TestWindowsSuspendOwnedJobProcessProof(t *testing.T) {
	for _, invalid := range []windows.Handle{0, windows.InvalidHandle} {
		if _, err := activeBrowserJobProcesses(invalid); err == nil {
			t.Fatal("invalid handle queried ambient job")
		}
	}
	private := trustedBaseDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsSuspendJobChild$")
	cmd.Dir = private
	cmd.Env = append(os.Environ(), "CITADEL_TEST_SUSPEND_JOB_CHILD=1", "HOME="+private,
		"USERPROFILE="+private, "XDG_CONFIG_HOME="+private, "XDG_CACHE_HOME="+private,
		"XDG_DATA_HOME="+private, "TMP="+private, "TEMP="+private)
	job, err := startInKillOnCloseJob(cmd)
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	closed := false
	defer func() {
		if !closed {
			_ = windows.CloseHandle(job)
		}
		waitBounded(exited)
	}()
	if n, err := activeBrowserJobProcesses(job); err != nil || n != 1 {
		t.Fatal("owned job did not account for its private test child", err)
	}
	s := newSuspendJobStop(exited, func() error { return stopJob(job, exited) },
		func() error { return windows.TerminateJobObject(job, 1) },
		func() (uint32, error) { return activeBrowserJobProcesses(job) },
		func() error {
			err := windows.CloseHandle(job)
			if err == nil {
				closed = true
			}
			return err
		})
	if err := s.stopForSuspend(suspendPagesContext(t)); err != nil {
		t.Fatal(err)
	}
	if err := s.ordinaryStop(); err != nil {
		t.Fatal(err)
	}
}

// Existing Windows real-browser CI runs this for Chrome and Edge.
// It exercises the installed proc hook, not a duplicate fake constructor or
// a public suspend action. Only this test's fresh throwaway browser is stopped.
func TestWindowsRealBrowserSuspendJobProof(t *testing.T) {
	requireBrowserIT(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	m.SetLaunchOptions(LaunchOptions{Quiet: true})
	st, err := m.StartSession("about:blank")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	m.mu.Lock()
	s := m.sessions[st.ID]
	m.mu.Unlock()
	if s == nil {
		t.Fatal("fresh browser session disappeared")
	}
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	if proc == nil || proc.stopForSuspend == nil {
		t.Fatal("fresh Windows runner lacks positive stop hook")
	}
	if err := proc.stopForSuspend(suspendPagesContext(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-proc.exited:
	default:
		t.Fatal("positive job proof preceded root reaping")
	}
	waitGone(t, proc.browserPID, 5*time.Second)
	if err := proc.stopForSuspend(suspendPagesContext(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.Profile); err != nil {
		t.Fatal("job hook removed the retained working profile")
	}
	if err := m.Stop(st.ID); err != nil {
		t.Fatal("ordinary cleanup failed after positive proof", err)
	}
}
