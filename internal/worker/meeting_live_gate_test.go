package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestPermissionRefusalsNameActionableToolsForEveryFilesAndDesktopType(t *testing.T) {
	for _, group := range []struct {
		types        []string
		reason, tool string
	}{
		{[]string{JobTypeFileRead, JobTypeFileReadBytes, JobTypeFileReadRangeV1, JobTypeFileReadRangeV2, JobTypeFileWrite, JobTypeFileWriteBytes, JobTypeFileEdit, JobTypeFileList, JobTypeFileSearch, JobTypeFileIndex, JobTypeFileSemanticSearch, JobTypeFileFetch}, "files_disabled", "citadel_set_files_permission"},
		{[]string{JobTypeFileScreenshot, JobTypeVNCScreenshot, JobTypeVNCType, JobTypeVNCKeys, JobTypeVNCActions}, "desktop_disabled", "citadel_set_desktop_permission"},
	} {
		for _, jobType := range group.types {
			t.Run(jobType, func(t *testing.T) {
				// Direct classification deliberately bypasses today's workspace
				// and upload registration order, protecting future reorderings.
				refusal := legacyPermissionRefusal(jobType)
				if refusal.Reason != group.reason || !strings.Contains(refusal.Message, group.tool) || !strings.Contains(refusal.Message, "enabled=true") || !strings.Contains(refusal.Message, "confirm=true") {
					t.Fatalf("missing actionable permission refusal: %v", refusal)
				}
			})
		}
	}
}

func TestMeetingJoinAndHuddleFollowOneLiveGateWithoutRebuildingHandlers(t *testing.T) {
	enabled := false
	runner := NewRunner(nil, CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
		WorkspaceDir: t.TempDir(), MeetingEnabled: func() bool { return enabled },
	}), RunnerConfig{})
	var inventory jobRefusalSource
	for _, handler := range runner.handlers {
		if source, ok := handler.(jobRefusalSource); ok {
			inventory = source
		}
	}
	if inventory == nil {
		t.Fatal("meeting refusal inventory missing")
	}
	for _, next := range []bool{false, true, false, true} {
		enabled = next
		for _, jobType := range []string{JobTypeMeetingJoin, JobTypeHuddleJoin} {
			refusal := inventory.RefusalFor(jobType)
			if runner.CanHandle(jobType) != enabled {
				t.Fatalf("%s dispatchability disagrees with live meeting gate %v", jobType, enabled)
			}
			// The refusal inventory must agree, even while an adapter remains
			// registered to support later re-enabling without a restart.
			if enabled && refusal != nil || !enabled && (refusal == nil || refusal.Reason != "meeting_disabled") {
				t.Fatalf("%s refusal disagrees with live gate %v: %v", jobType, enabled, refusal)
			}
		}
		if !runner.CanHandle(JobTypeHuddleTeardown) || !runner.CanHandle(JobTypeTranscribeAudio) {
			t.Fatal("disabling new joins removed teardown or existing audio transcription")
		}
	}
	withoutWorkspace := NewRunner(nil, CreateLegacyHandlersWithOpts(LegacyHandlerOpts{MeetingEnabled: func() bool { return true }}), RunnerConfig{})
	if withoutWorkspace.CanHandle(JobTypeMeetingJoin) || !withoutWorkspace.CanHandle(JobTypeHuddleJoin) {
		t.Fatal("live meeting gate removed the recording workspace boundary")
	}
	if refusal := withoutWorkspace.unavailableJobRefusal(JobTypeMeetingJoin); refusal == nil || refusal.Reason != "workspace_not_configured" {
		t.Fatal("missing workspace was misclassified as permission disabled")
	}
}

func TestRegisteredMeetingJoinRechecksGateBeforeFakeHandler(t *testing.T) {
	enabled := true
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{WorkspaceDir: t.TempDir(), MeetingEnabled: func() bool { return enabled }})
	var adapter *LegacyHandlerAdapter
	for _, candidate := range handlers {
		if h, ok := candidate.(*LegacyHandlerAdapter); ok && h.jobType == JobTypeMeetingJoin {
			adapter = h
			break
		}
	}
	if adapter == nil || !adapter.CanHandle(JobTypeMeetingJoin) {
		t.Fatal("enabled meeting adapter missing")
	}
	// Replace the implementation before Execute. No real meeting, service,
	// browser, audio or network effect can occur, including under mutation.
	fake := &TestLegacyHandler{output: `{"success":true}`}
	adapter.handler = fake
	enabled = false
	result, err := adapter.Execute(context.Background(), &Job{ID: "revoked", Type: JobTypeMeetingJoin, Payload: map[string]any{"meeting_url": "fixture-must-not-be-used"}}, &NoOpStreamWriter{})
	var refusal *jobs.ShellRefusal
	if !errors.As(err, &refusal) || refusal.Reason != "meeting_disabled" || result == nil || result.Status != JobStatusFailure || fake.capturedPayload != nil {
		t.Fatalf("registration did not enforce live revocation before execution: result=%+v err=%v", result, err)
	}
}

func TestMeetingDefaultGateReloadsOnlyIsolatedPersistedConfiguration(t *testing.T) {
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(key, home)
	}
	t.Setenv("SUDO_USER", "")
	// Root's ConfigDir selection requires a config marker before it selects
	// HOME. Establish it inside the fixture before calling the path resolver.
	local := filepath.Join(home, ".citadel-cli")
	if err := os.MkdirAll(local, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "config.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := platform.ConfigDir()
	rel, err := filepath.Rel(home, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatal("fixture resolved a real host config path before any effect")
	}
	write := func(enabled bool) {
		t.Helper()
		settings := config.DefaultMeeting()
		settings.MeetingEnabled = enabled
		if err := config.SaveMeeting(dir, settings); err != nil {
			t.Fatal(err)
		}
	}
	write(false)
	runner := NewRunner(nil, CreateLegacyHandlersWithOpts(LegacyHandlerOpts{WorkspaceDir: t.TempDir()}), RunnerConfig{})
	for _, enabled := range []bool{false, true, false} {
		write(enabled)
		for _, jobType := range []string{JobTypeMeetingJoin, JobTypeHuddleJoin} {
			if runner.CanHandle(jobType) != enabled {
				t.Fatalf("%s retained stale persisted meeting configuration", jobType)
			}
		}
	}
}
