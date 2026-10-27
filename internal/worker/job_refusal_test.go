package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

func TestGatedJobRefusalIsTerminalFromFirstAttempt(t *testing.T) {
	for _, tc := range []struct {
		name, jobType, reason, message string
		opts                           LegacyHandlerOpts
	}{
		{"shell snapshot disabled", JobTypeShellCommand, "shell_disabled", "enable Shell", LegacyHandlerOpts{GOOS: "linux", ShellDisabled: true}},
		{"shell live disabled", JobTypeShellCommand, "shell_disabled", "enable Shell", LegacyHandlerOpts{GOOS: "linux", ShellEnabled: func() bool { return false }}},
		{"native windows", JobTypeShellCommand, "platform_unsupported", "native Windows", LegacyHandlerOpts{GOOS: "windows", ShellEnabled: func() bool { return true }}},
		{"desktop disabled", JobTypeVNCActions, "desktop_disabled", "enable Desktop", LegacyHandlerOpts{DesktopDisabled: true}},
		{"desktop live disabled", JobTypeFileScreenshot, "desktop_disabled", "enable Desktop", LegacyHandlerOpts{DesktopEnabled: func() bool { return false }}},
		{"files disabled", JobTypeFileRead, "files_disabled", "enable Files", LegacyHandlerOpts{WorkspaceDir: "fixture", FilesDisabled: true}},
		{"files live disabled", JobTypeFileReadRangeV1, "files_disabled", "enable Files", LegacyHandlerOpts{WorkspaceDir: "fixture", FilesEnabled: func() bool { return false }}},
		{"workspace absent", JobTypeFileRead, "workspace_not_configured", "workspace", LegacyHandlerOpts{}},
		{"upload workspace absent", JobTypeFileWriteBytes, "workspace_not_configured", "workspace", LegacyHandlerOpts{}},
		{"transcription workspace absent", JobTypeTranscribeAudio, "workspace_not_configured", "workspace", LegacyHandlerOpts{}},
		{"manifest absent", JobTypeServiceStart, "config_not_configured", "manifest", LegacyHandlerOpts{}},
		{"meeting disabled", JobTypeHuddleJoin, "meeting_disabled", "meeting capability", LegacyHandlerOpts{MeetingEnabled: func() bool { return false }}},
		{"fetch config absent", JobTypeFileFetch, "capability_unavailable", "trusted HTTPS", LegacyHandlerOpts{WorkspaceDir: "fixture", FilesEnabled: func() bool { return true }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			if opts.WorkspaceDir != "" {
				opts.WorkspaceDir = t.TempDir()
			}
			opts.FileFetchOrigin = func() string { return "" }
			if opts.MeetingEnabled == nil {
				opts.MeetingEnabled = func() bool { return false }
			}
			handlers := CreateLegacyHandlersWithOpts(opts)
			source := NewMockJobSource("fake", nil)
			runner := NewRunner(source, handlers, RunnerConfig{AgentVersion: "v2.201.0", ActivityFn: func(string, string) {}})
			stream := &MockStreamWriter{}
			runner.WithStreamWriterFactory(func(*Job) StreamWriter { return stream })
			job := &Job{ID: "refused-fixture", Type: tc.jobType, Payload: map[string]any{"command": "must never execute", "path": "/must-not-read"}, Metadata: JobMetadata{Attempts: 1, MaxAttempts: 5}}
			if runner.CanHandle(job.Type) {
				t.Fatalf("withheld job advertised: %s", job.Type)
			}
			if runner.executeJob(context.Background(), job, stream, time.Now(), false, 0) {
				t.Fatal("refused job succeeded")
			}
			var refusal *jobs.ShellRefusal
			if !errors.As(stream.erroredErr, &refusal) || refusal.Reason != tc.reason || !strings.Contains(refusal.Message, tc.message) || strings.Contains(refusal.Message, "update the node") {
				t.Fatalf("wrong typed gate reason: %v", stream.erroredErr)
			}
			var encoded map[string]string
			if err := json.Unmarshal([]byte(stream.erroredErr.Error()), &encoded); err != nil || encoded["reason"] != tc.reason {
				t.Fatalf("refusal contract invalid: %v %v", encoded, err)
			}
			if stream.errorCount != 1 || stream.erroredRecover || stream.ended || len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 || len(source.AckedJobs()) != 0 {
				t.Fatalf("not terminal on first attempt: stream=%+v failed=%d nacked=%d acked=%d", stream, len(source.FailedJobs()), len(source.NackedJobs()), len(source.AckedJobs()))
			}
			data := source.FailedData()[0]
			if data["reason"] != tc.reason || data["refused"] != true || data["known_to_build"] != true || data["unsupported_job_type"] != false || data["job_type"] != tc.jobType || data["agent_version"] != "v2.201.0" || data["unregistered_reason"] != refusal.Message {
				t.Fatalf("gate metadata wrong: %+v", data)
			}
		})
	}
}

func TestUnknownJobRefusalRetainsUpdateHintOnlyForUnknown(t *testing.T) {
	for _, jobType := range []string{"FUTURE_UNKNOWN_JOB", JobTypeCobrowse} {
		source := NewMockJobSource("fake", nil)
		runner := NewRunner(source, nil, RunnerConfig{AgentVersion: "v2.201.0", ActivityFn: func(string, string) {}})
		stream := &MockStreamWriter{}
		runner.WithStreamWriterFactory(func(*Job) StreamWriter { return stream })
		runner.executeJob(context.Background(), &Job{ID: "fixture", Type: jobType}, stream, time.Now(), false, 0)
		unknown := jobType == "FUTURE_UNKNOWN_JOB"
		if strings.Contains(stream.erroredErr.Error(), "update the node") != unknown || source.FailedData()[0]["unsupported_job_type"] != unknown {
			t.Fatalf("known/unknown conflated: %v %+v", stream.erroredErr, source.FailedData())
		}
		if unknown {
			if _, present := source.FailedData()[0]["refused"]; present {
				t.Fatal("unknown type claimed as permission refusal")
			}
			if !strings.Contains(stream.erroredErr.Error(), "v2.201.0") {
				t.Fatal("unknown version hint lost")
			}
		}
	}
}

func TestRegistrationRefusalsTrackLiveOverridesWithoutGrantingWork(t *testing.T) {
	enabled := false
	g := &registrationRefusals{goos: "linux", opts: LegacyHandlerOpts{ShellDisabled: true, ShellEnabled: func() bool { return enabled }}}
	if g.RefusalFor(JobTypeShellCommand).Reason != "shell_disabled" {
		t.Fatal("live false did not refuse")
	}
	enabled = true
	if g.RefusalFor(JobTypeShellCommand) != nil {
		t.Fatal("startup snapshot overrode live enable")
	}
	for _, jobType := range append(append([]string{}, allKnownJobTypes...), "UNKNOWN") {
		if g.CanHandle(jobType) {
			t.Fatal("refusal inventory advertised execution authority")
		}
	}
	enabled = false
	if g.RefusalFor(JobTypeShellCommand).Reason != "shell_disabled" {
		t.Fatal("live revocation not seen")
	}
	if g.RefusalFor("UNKNOWN") != nil {
		t.Fatal("unknown job misclassified")
	}
}

func TestLegacyPermissionRevocationAfterSelectionIsTerminal(t *testing.T) {
	for _, jobType := range []string{JobTypeShellCommand, JobTypeFileRead, JobTypeVNCActions} {
		t.Run(jobType, func(t *testing.T) {
			calls := 0
			legacy := &TestLegacyHandler{output: `{"success":true}`}
			h := newGatedLegacyHandlerAdapter(jobType, legacy, func() bool { calls++; return calls == 1 })
			source := NewMockJobSource("fake", nil)
			runner := NewRunner(source, []JobHandler{h}, RunnerConfig{ActivityFn: func(string, string) {}})
			stream := &MockStreamWriter{}
			runner.WithStreamWriterFactory(func(*Job) StreamWriter { return stream })
			job := &Job{ID: "revoked", Type: jobType, Payload: map[string]any{"command": "must not execute"}, Metadata: JobMetadata{Attempts: 1, MaxAttempts: 5}}
			if runner.executeJob(context.Background(), job, stream, time.Now(), false, 0) {
				t.Fatal("revoked job succeeded")
			}
			var refusal *jobs.ShellRefusal
			if !errors.As(stream.erroredErr, &refusal) || len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 || stream.errorCount != 1 || legacy.capturedPayload != nil {
				t.Fatalf("revocation retried or executed: %v %+v", stream.erroredErr, source.FailedData())
			}
			if source.FailedData()[0]["reason"] != legacyPermissionRefusal(jobType).Reason {
				t.Fatal("revocation reason lost")
			}
		})
	}
}
