package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

const testNodePasscode = "secret-4821"

// recordingDesktopHandler stands in for a desktop handler and records the
// payload it was given.
type recordingDesktopHandler struct {
	mu       sync.Mutex
	payloads []map[string]string
}

func (h *recordingDesktopHandler) Execute(_ jobs.JobContext, job *nexus.Job) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	copied := map[string]string{}
	for k, v := range job.Payload {
		copied[k] = v
	}
	h.payloads = append(h.payloads, copied)
	return []byte("ok"), nil
}

func (h *recordingDesktopHandler) calls() []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.payloads
}

func passcodeState(hasPasscode bool) func() (jobs.PasscodeState, error) {
	return func() (jobs.PasscodeState, error) {
		return jobs.PasscodeState{Set: hasPasscode, Check: jobs.BoolCheck(func(pin string) bool { return pin == testNodePasscode })}, nil
	}
}

func passcodeGate(hasPasscode bool) jobs.DesktopGate {
	return jobs.DesktopGate{Load: passcodeState(hasPasscode)}
}

func TestDesktopAdapterHonorsNodePasscode(t *testing.T) {
	enabled := func() bool { return true }
	for _, tc := range []struct {
		name        string
		hasPasscode bool
		payload     map[string]any
		wantRefused bool
	}{
		{"no passcode set runs unchanged", false, map[string]any{"actions": "[]"}, false},
		{"passcode set and absent is refused", true, map[string]any{"actions": "[]"}, true},
		{"passcode set and wrong is refused", true, map[string]any{"actions": "[]", "passcode": "0000"}, true},
		{"passcode set and correct runs", true, map[string]any{"actions": "[]", "passcode": testNodePasscode}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := &recordingDesktopHandler{}
			var logs []string
			a := newDesktopLegacyHandlerAdapter(JobTypeVNCActions, handler, enabled, passcodeGate(tc.hasPasscode))
			a.SetLogFn(func(level, msg string) { logs = append(logs, level+" "+msg) })
			result, err := a.Execute(context.Background(), &Job{ID: "d1", Type: JobTypeVNCActions, Payload: tc.payload}, nil)

			if tc.wantRefused {
				var refusal *jobs.ShellRefusal
				if !errors.As(err, &refusal) || refusal.Reason != jobs.ReasonPasscodeInvalid {
					t.Fatalf("err = %v, want a passcode_invalid refusal", err)
				}
				if result == nil || result.Status != JobStatusFailure {
					t.Fatalf("result = %+v, want a failure result", result)
				}
				if len(handler.calls()) != 0 {
					t.Fatal("a refused desktop job must never reach the handler")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				calls := handler.calls()
				if len(calls) != 1 {
					t.Fatalf("handler calls = %d, want 1", len(calls))
				}
				if _, ok := calls[0][jobs.ShellPasscodePayloadKey]; ok {
					t.Fatal("the payload passcode must not reach the desktop handler")
				}
				if calls[0]["actions"] != "[]" {
					t.Fatalf("handler payload lost its other fields: %#v", calls[0])
				}
			}
			for _, line := range logs {
				if strings.Contains(line, testNodePasscode) {
					t.Fatalf("log line leaks the passcode: %q", line)
				}
			}
			if err != nil && strings.Contains(err.Error(), testNodePasscode) {
				t.Fatalf("error leaks the passcode: %v", err)
			}
		})
	}
}

func TestDesktopAdapterPermissionGateRunsFirst(t *testing.T) {
	handler := &recordingDesktopHandler{}
	gate := jobs.DesktopGate{Load: func() (jobs.PasscodeState, error) {
		t.Fatal("disabled desktop permission reached the passcode gate")
		return jobs.PasscodeState{}, nil
	}}
	a := newDesktopLegacyHandlerAdapter(JobTypeVNCKeys, handler, func() bool { return false }, gate)
	_, err := a.Execute(context.Background(), &Job{ID: "d2", Type: JobTypeVNCKeys, Payload: map[string]any{"passcode": testNodePasscode}}, nil)
	var refusal *jobs.ShellRefusal
	if !errors.As(err, &refusal) || refusal.Reason != "desktop_disabled" {
		t.Fatalf("a disabled desktop permission must refuse before the passcode gate, got %v", err)
	}
	if len(handler.calls()) != 0 {
		t.Fatal("a disabled desktop job must never reach the handler")
	}
}

func TestCreateLegacyHandlersGatesEveryDesktopJobOnPasscode(t *testing.T) {
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
		DesktopEnabled:       func() bool { return true },
		DesktopPasscodeState: passcodeState(true),
	})
	for _, jobType := range []string{JobTypeFileScreenshot, JobTypeVNCScreenshot, JobTypeVNCType, JobTypeVNCKeys, JobTypeVNCActions} {
		var h JobHandler
		for _, candidate := range handlers {
			if candidate.CanHandle(jobType) {
				h = candidate
				break
			}
		}
		if h == nil {
			t.Fatalf("%s is not registered with desktop enabled", jobType)
		}
		_, err := h.Execute(context.Background(), &Job{ID: "g-" + jobType, Type: jobType, Payload: map[string]any{}}, nil)
		var refusal *jobs.ShellRefusal
		if !errors.As(err, &refusal) || refusal.Reason != jobs.ReasonPasscodeInvalid {
			t.Fatalf("%s without the passcode: err = %v, want passcode_invalid", jobType, err)
		}
	}
}

func TestCreateLegacyHandlersDesktopFailsClosedWithoutPasscodeWiring(t *testing.T) {
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{DesktopEnabled: func() bool { return true }})
	for _, candidate := range handlers {
		if !candidate.CanHandle(JobTypeVNCActions) {
			continue
		}
		_, err := candidate.Execute(context.Background(), &Job{ID: "nil-gate", Type: JobTypeVNCActions, Payload: map[string]any{}}, nil)
		var refusal *jobs.ShellRefusal
		if !errors.As(err, &refusal) || refusal.Reason != jobs.ReasonPasscodeUnavailable {
			t.Fatalf("unwired passcode gate must refuse with passcode_unavailable, got %v", err)
		}
		return
	}
	t.Fatal("VNC_ACTIONS not registered")
}

func TestRunnerPasscodeRefusalIsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata JobMetadata
	}{
		{"first_redis_delivery", JobMetadata{Attempts: 1, MaxAttempts: 3}},
		{"without_delivery_metadata", JobMetadata{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := NewMockJobSource("test", nil)
			handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
				DesktopEnabled:       func() bool { return true },
				DesktopPasscodeState: passcodeState(true),
			})
			runner := NewRunner(source, handlers, RunnerConfig{WorkerID: "test-worker"})
			stream := &MockStreamWriter{}
			job := &Job{ID: "refused-desktop", Type: JobTypeVNCScreenshot, Payload: map[string]any{"passcode": "wrong"}, Metadata: tc.metadata}

			if runner.executeJob(context.Background(), job, stream, time.Now(), false, 0) {
				t.Fatal("a passcode refusal must not report job success")
			}
			if stream.errorCount != 1 || stream.endCount != 0 || stream.erroredRecover {
				t.Fatalf("want exactly one non-recoverable error: errors=%d ends=%d recoverable=%v", stream.errorCount, stream.endCount, stream.erroredRecover)
			}
			var refusal struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(stream.erroredErr.Error()), &refusal); err != nil || refusal.Reason != jobs.ReasonPasscodeInvalid {
				t.Fatalf("terminal event lost the typed refusal: reason=%q err=%v", refusal.Reason, err)
			}
			if len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 || len(source.AckedJobs()) != 0 {
				t.Fatalf("want source.Fail, never Nack or success Ack: failed=%d nacked=%d acked=%d", len(source.FailedJobs()), len(source.NackedJobs()), len(source.AckedJobs()))
			}
			if source.FailedData()[0]["reason"] != jobs.ReasonPasscodeInvalid {
				t.Fatalf("failed-job metadata lost the reason: %#v", source.FailedData())
			}
		})
	}
}

func TestRunnerShellPasscodeRefusalIsTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SHELL_COMMAND is not registered on Windows")
	}
	source := NewMockJobSource("test", nil)
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
		ShellEnabled:        func() bool { return true },
		ShellHasPasscode:    func() bool { return true },
		ShellVerifyPasscode: func(pin string) bool { return pin == testNodePasscode },
	})
	runner := NewRunner(source, handlers, RunnerConfig{WorkerID: "test-worker"})
	stream := &MockStreamWriter{}
	job := &Job{ID: "refused-shell", Type: JobTypeShellCommand, Payload: map[string]any{"command": "true"}, Metadata: JobMetadata{Attempts: 1, MaxAttempts: 3}}
	if runner.executeJob(context.Background(), job, stream, time.Now(), false, 0) {
		t.Fatal("a passcode refusal must not report job success")
	}
	if stream.errorCount != 1 || len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 {
		t.Fatalf("shell refusal must be terminal: errors=%d failed=%d nacked=%d", stream.errorCount, len(source.FailedJobs()), len(source.NackedJobs()))
	}
}

func TestDesktopAdapterRefusesWhenPasscodeStateUnreadable(t *testing.T) {
	handler := &recordingDesktopHandler{}
	unreadable := jobs.DesktopGate{Load: func() (jobs.PasscodeState, error) {
		return jobs.PasscodeState{}, errors.New("read permissions: permission denied")
	}}
	a := newDesktopLegacyHandlerAdapter(JobTypeFileScreenshot, handler, func() bool { return true }, unreadable)
	_, err := a.Execute(context.Background(), &Job{ID: "u", Type: JobTypeFileScreenshot, Payload: map[string]any{}}, nil)
	var refusal *jobs.ShellRefusal
	if !errors.As(err, &refusal) || refusal.Reason != jobs.ReasonPasscodeUnavailable {
		t.Fatalf("an unreadable passcode state must refuse, got %v", err)
	}
	if len(handler.calls()) != 0 {
		t.Fatal("a refused desktop job must never reach the handler")
	}
}

func TestWorkerJobRenderingOmitsTheNodePasscode(t *testing.T) {
	job := Job{ID: "j", Type: JobTypeVNCActions, Payload: map[string]any{"passcode": testNodePasscode, "actions": "[]"}, RawPayload: []byte(testNodePasscode)}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{string(encoded), fmt.Sprintf("%v", job), fmt.Sprintf("%+v", job)} {
		if strings.Contains(rendered, testNodePasscode) {
			t.Fatalf("job rendering retained the passcode: %s", rendered)
		}
	}
}

func TestLegacyHandlersShareOnePasscodeThrottle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SHELL_COMMAND is not registered on Windows")
	}
	throttle := jobs.NewPasscodeThrottle()
	verify := func(pin string) bool { return pin == testNodePasscode }
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
		ShellEnabled: func() bool { return true }, ShellHasPasscode: func() bool { return true }, ShellVerifyPasscode: verify,
		DesktopEnabled: func() bool { return true }, DesktopPasscodeState: passcodeState(true),
		PasscodeThrottle: throttle,
	})
	find := func(jobType string) JobHandler {
		for _, h := range handlers {
			if h.CanHandle(jobType) {
				return h
			}
		}
		t.Fatalf("%s not registered", jobType)
		return nil
	}
	shell, desktop := find(JobTypeShellCommand), find(JobTypeVNCActions)
	for i := 0; i < jobs.DefaultPasscodeMaxFailures; i++ {
		_, _ = shell.Execute(context.Background(), &Job{ID: "s", Type: JobTypeShellCommand, Payload: map[string]any{"command": "true", "passcode": "wrong"}}, nil)
	}
	_, err := desktop.Execute(context.Background(), &Job{ID: "d", Type: JobTypeVNCActions, Payload: map[string]any{"passcode": testNodePasscode}}, nil)
	var refusal *jobs.ShellRefusal
	if !errors.As(err, &refusal) || refusal.Reason != jobs.ReasonPasscodeThrottled {
		t.Fatalf("shell failures must throttle desktop jobs too, got %v", err)
	}
}

func TestShellPasscodeCheckReportsLockout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SHELL_COMMAND is not registered on Windows")
	}
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
		ShellEnabled: func() bool { return true }, ShellHasPasscode: func() bool { return true },
		ShellVerifyPasscode: func(string) bool { return false },
		ShellPasscodeState: func() (jobs.PasscodeState, error) {
			return jobs.PasscodeState{Set: true, Check: func(string) jobs.PasscodeVerdict { return jobs.PasscodeLockedOut }}, nil
		},
	})
	for _, h := range handlers {
		if !h.CanHandle(JobTypeShellCommand) {
			continue
		}
		_, err := h.Execute(context.Background(), &Job{ID: "l", Type: JobTypeShellCommand, Payload: map[string]any{"command": "true", "passcode": testNodePasscode}}, nil)
		var refusal *jobs.ShellRefusal
		if !errors.As(err, &refusal) || refusal.Reason != jobs.ReasonPasscodeLocked {
			t.Fatalf("a vault lockout must refuse as passcode_locked, got %v", err)
		}
		return
	}
	t.Fatal("SHELL_COMMAND not registered")
}
