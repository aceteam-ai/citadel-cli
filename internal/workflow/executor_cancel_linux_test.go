//go:build linux

package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type ownedWorkflowProcess struct {
	pid   int
	group int
	start string
	name  string
	state string
}

func readOwnedWorkflowProcess(pid int) (ownedWorkflowProcess, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ownedWorkflowProcess{}, err
	}
	text := string(data)
	open, close := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if open < 0 || close <= open {
		return ownedWorkflowProcess{}, fmt.Errorf("invalid owned process stat")
	}
	fields := strings.Fields(text[close+1:])
	if len(fields) <= 19 {
		return ownedWorkflowProcess{}, fmt.Errorf("short owned process stat")
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return ownedWorkflowProcess{}, err
	}
	return ownedWorkflowProcess{pid, group, fields[19], text[open+1 : close], fields[0]}, nil
}

func awaitOwnedWorkflowProcessExit(t *testing.T, process ownedWorkflowProcess, allowZombie bool) {
	t.Helper()
	if process.pid <= 1 || process.start == "" {
		t.Fatal("missing captured owned process identity")
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := readOwnedWorkflowProcess(process.pid)
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) || err == nil && current.start != process.start {
			return // The exact original process is gone, not a reused PID.
		}
		if err != nil {
			t.Fatal(err)
		}
		// A killed orphan can await init's reap. The direct shell/exec and
		// the pre-cancellation leader barrier still require actual reaping.
		if allowZombie && (current.state == "Z" || current.state == "X") {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("exact original owned process did not exit/reap before observation deadline")
		case <-ticker.C:
		}
	}
}

func awaitOwnedWorkflowSleeper(t *testing.T, marker string, count int, owned *[]ownedWorkflowProcess) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if raw, err := os.ReadFile(marker); err == nil {
			ids := strings.Fields(string(raw))
			if len(ids) == count {
				processes := make([]ownedWorkflowProcess, 0, count)
				for _, id := range ids {
					pid, err := strconv.Atoi(id)
					if err != nil || pid <= 1 {
						t.Fatal("invalid owned fixture PID")
					}
					process, err := readOwnedWorkflowProcess(pid)
					if err != nil {
						break
					}
					processes = append(processes, process)
				}
				if len(processes) == count {
					// Capture PID+start-time before any readiness assertion,
					// so a failed regression can still clean only its own work.
					*owned = processes
					ready := processes[count-1].name == "sleep"
					for _, process := range processes {
						ready = ready && process.group == processes[0].pid && process.state != "Z"
					}
					if ready {
						return
					}
				}
			}
		}
		select {
		case <-timer.C:
			t.Fatal("owned shell/sleeper did not reach its dedicated-group barrier")
		case <-ticker.C:
		}
	}
}

func TestExecutorCancelStopsOwnedSleepingShellAndChild(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(fmt.Sprintf("child=%v", child), func(t *testing.T) {
			base := t.TempDir()
			marker := filepath.Join(base, "owned-pids")
			quoted := "'" + strings.ReplaceAll(marker, "'", "'\\''") + "'"
			command := "printf '%s' \"$$\" > " + quoted + "; exec sleep 60"
			count := 1
			if child {
				command = "sleep 60 & printf '%s %s' \"$$\" \"$!\" > " + quoted + "; wait"
				count = 2
			}
			e := NewExecutor(ExecutorConfig{
				DefaultTimeout: time.Minute,
				Shell:          ShellConfig{Gate: passcodeGate(true), WorkspaceDir: base},
			})
			ctx, cancel := context.WithCancel(context.Background())
			run, err := e.Submit(ctx, &RunRequest{Graph: shellGraph(command), Passcode: testPasscode})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			var owned []ownedWorkflowProcess
			t.Cleanup(func() {
				cancel()
				// Regression failures must not leave a 60s child behind.
				// Never signal a PID that no longer has the captured birth ID.
				for i := len(owned) - 1; i >= 0; i-- {
					process := owned[i]
					if current, err := readOwnedWorkflowProcess(process.pid); err == nil && current.start == process.start {
						_ = syscall.Kill(process.pid, syscall.SIGKILL)
					}
				}
				select {
				case <-run.Done():
				case <-time.After(5 * time.Second):
					t.Error("owned fixture did not finish cleanup")
				}
			})
			awaitOwnedWorkflowSleeper(t, marker, count, &owned)
			if !e.Cancel(run.ID) {
				t.Fatal("running sleeping Shell was not cancelled")
			}
			awaitWorkflowSignal(t, run.Done())
			if run.Snapshot().Status != StatusCancelled || e.ActiveCount() != 0 {
				t.Fatal("shell cancellation was overwritten or kept its run slot")
			}
			for i, process := range owned {
				awaitOwnedWorkflowProcessExit(t, process, i > 0)
			}
		})
	}
}

func awaitOwnedWorkflowShellReaped(t *testing.T, leader ownedWorkflowProcess, child ownedWorkflowProcess) {
	t.Helper()
	awaitOwnedWorkflowProcessExit(t, leader, false)
	background, err := readOwnedWorkflowProcess(child.pid)
	if err != nil || background.start != child.start || background.group != leader.pid || background.state == "Z" || background.state == "X" {
		t.Fatal("original pipe-holding background child did not survive shell reap")
	}
}

func TestExecutorCancellationAfterShellReapStopsPipeHoldingChild(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%v", timeout), func(t *testing.T) {
			base := t.TempDir()
			marker := filepath.Join(base, "owned-pids")
			release := filepath.Join(base, "release-shell")
			quote := func(path string) string { return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'" }
			command := "sleep 60 & printf '%s %s' \"$$\" \"$!\" > " + quote(marker) +
				"; while [ ! -f " + quote(release) + " ]; do sleep 0.01; done; echo started"
			e := NewExecutor(ExecutorConfig{
				DefaultTimeout: 2 * time.Second,
				Shell:          ShellConfig{Gate: passcodeGate(true), WorkspaceDir: base},
			})
			ctx, cancel := context.WithCancel(context.Background())
			run, err := e.Submit(ctx, &RunRequest{Graph: shellGraph(command), Passcode: testPasscode})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			var owned []ownedWorkflowProcess
			t.Cleanup(func() {
				cancel()
				for i := len(owned) - 1; i >= 0; i-- {
					process := owned[i]
					if current, err := readOwnedWorkflowProcess(process.pid); err == nil && current.start == process.start {
						_ = syscall.Kill(process.pid, syscall.SIGKILL)
					}
				}
				select {
				case <-run.Done():
				case <-time.After(5 * time.Second):
					t.Error("owned background fixture did not finish cleanup")
				}
			})
			awaitOwnedWorkflowSleeper(t, marker, 2, &owned)
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			awaitOwnedWorkflowShellReaped(t, owned[0], owned[1])
			select {
			case <-run.Done():
				t.Fatal("run completed while the original child still held its output pipe")
			default:
			}
			if !timeout && !e.Cancel(run.ID) {
				t.Fatal("running pipe-holding child was not cancelled")
			}
			wait := 2 * time.Second
			if timeout {
				wait = 4 * time.Second
			}
			select {
			case <-run.Done():
			case <-time.After(wait):
				t.Fatal("cancel or run timeout did not stop the pipe-holding child")
			}
			want := StatusCancelled
			if timeout {
				want = StatusFailed
			}
			if run.Snapshot().Status != want || e.ActiveCount() != 0 {
				t.Fatal("cancel/timeout was overwritten or retained its executor slot")
			}
			awaitOwnedWorkflowProcessExit(t, owned[1], true)
		})
	}
}

func TestExecutorUncancelledBackgroundWriterKeepsCompleteOutput(t *testing.T) {
	e := NewExecutor(ExecutorConfig{
		DefaultTimeout: 10 * time.Second,
		Shell:          ShellConfig{Gate: passcodeGate(true)},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, err := e.Submit(ctx, &RunRequest{
		Graph: shellGraph("printf 'started-'; (sleep 0.05; printf 'late-output') & exit 0"), Passcode: testPasscode,
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitWorkflowSignal(t, run.Done())
	snapshot := run.Snapshot()
	if snapshot.Status != StatusCompleted || snapshot.Output["stdout"] != "started-late-output" || e.ActiveCount() != 0 {
		t.Fatal("ordinary background output was truncated or treated as cancelled")
	}
}
