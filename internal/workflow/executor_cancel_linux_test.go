//go:build linux

package workflow

import (
	"context"
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
				current, err := readOwnedWorkflowProcess(process.pid)
				if os.IsNotExist(err) || err == nil && current.start != process.start {
					continue // Exact original process has been reaped.
				}
				if err != nil {
					t.Fatal(err)
				}
				// A child killed together with its shell can briefly await
				// init's reap. It is dead, not still executing or sleeping.
				// The direct shell/executable is always reaped by Cmd.Wait.
				if i > 0 && (current.state == "Z" || current.state == "X") {
					continue
				}
				t.Fatal("owned sleeping process survived cancellation")
			}
		})
	}
}
