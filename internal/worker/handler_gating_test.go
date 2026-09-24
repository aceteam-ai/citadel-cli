package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// anyHandles reports whether the built handler set registers a handler for the
// given job type.
func anyHandles(handlers []JobHandler, jobType string) bool {
	for _, h := range handlers {
		if h.CanHandle(jobType) {
			return true
		}
	}
	return false
}

// TestFreshNode_RefusesSensitiveJobs is the aceteam#6524 teeth: a fresh node
// (sensitive surfaces default-DENY, wired as DesktopDisabled/FilesDisabled) must
// refuse screen/VNC and file-browse jobs. Binary writes stay registered only
// to return an actionable permission refusal without touching the workspace.
func TestFreshNode_RefusesSensitiveJobs(t *testing.T) {
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
		WorkspaceDir:    t.TempDir(), // a workspace IS configured...
		DesktopDisabled: true,        // ...but desktop is off
		FilesDisabled:   true,        // ...and files is off
	})

	desktopJobs := []string{
		JobTypeFileScreenshot, JobTypeVNCScreenshot,
		JobTypeVNCType, JobTypeVNCKeys, JobTypeVNCActions,
	}
	for _, jt := range desktopJobs {
		if anyHandles(handlers, jt) {
			t.Errorf("fresh node must NOT register desktop job %q", jt)
		}
	}

	fileJobs := []string{
		JobTypeFileRead, JobTypeFileReadBytes, JobTypeFileWrite,
		JobTypeFileEdit, JobTypeFileList,
		JobTypeFileSearch, JobTypeFileIndex, JobTypeFileSemanticSearch,
	}
	for _, jt := range fileJobs {
		if anyHandles(handlers, jt) {
			t.Errorf("fresh node must NOT register file-browse job %q", jt)
		}
	}
}

// TestFreshNode_StillServesInferenceAndMeeting proves the gate did not
// over-reach: inference and the default-ON meeting/transcribe/TTS surfaces stay
// available even with console/desktop/files disabled. Joining to serve a model
// must never require enabling remote access.
func TestFreshNode_StillServesInferenceAndMeeting(t *testing.T) {
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
		WorkspaceDir:    t.TempDir(),
		DesktopDisabled: true,
		FilesDisabled:   true,
	})

	mustHandle := []string{
		JobTypeVLLMInference,
		JobTypeOllamaInference,
		JobTypeLlamaCppInference,
		JobTypeEmbedding,
		JobTypeSynthesizeSpeech,
		JobTypeMediaGenerate,
		JobTypeTranscribeAudio, // meeting capability, shares the workspace but not gated
	}
	for _, jt := range mustHandle {
		if !anyHandles(handlers, jt) {
			t.Errorf("inference/meeting job %q must be served regardless of console/desktop/files", jt)
		}
	}
}

// TestEnabledNode_RegistersSensitiveHandlers confirms opting in (Desktop/Files
// enabled) restores the handlers.
func TestEnabledNode_RegistersSensitiveHandlers(t *testing.T) {
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
		WorkspaceDir:    t.TempDir(),
		DesktopDisabled: false,
		FilesDisabled:   false,
	})
	if !anyHandles(handlers, JobTypeVNCScreenshot) {
		t.Error("enabled node should register VNC_SCREENSHOT")
	}
	if !anyHandles(handlers, JobTypeFileRead) {
		t.Error("enabled node should register FILE_READ")
	}
}

func TestFilesDisabled_BinaryWriteReturnsPermissionRefusal(t *testing.T) {
	workspace := t.TempDir()
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
		WorkspaceDir:  workspace,
		FilesDisabled: true,
	})
	for _, handler := range handlers {
		if !handler.CanHandle(JobTypeFileWriteBytes) {
			continue
		}
		result, err := handler.Execute(context.Background(), &Job{
			ID:      "denied-write",
			Type:    JobTypeFileWriteBytes,
			Payload: map[string]any{"path": "blocked.bin", "content": "YWJj"},
		}, &NoOpStreamWriter{})
		if err == nil || result == nil || result.Status != JobStatusFailure {
			t.Fatalf("disabled binary write must fail: result=%+v err=%v", result, err)
		}
		var refusal struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		if decodeErr := json.Unmarshal([]byte(err.Error()), &refusal); decodeErr != nil {
			t.Fatalf("refusal must be machine-readable: %v", decodeErr)
		}
		if refusal.Reason != "files_disabled" || !strings.Contains(refusal.Message, "Files permission required") || !strings.Contains(refusal.Message, "citadel_set_files_permission") || !strings.Contains(refusal.Message, "restart") {
			t.Fatalf("missing permission diagnosis or recovery: %+v", refusal)
		}
		if _, statErr := os.Stat(filepath.Join(workspace, "blocked.bin")); !os.IsNotExist(statErr) {
			t.Fatalf("refused write touched the workspace: %v", statErr)
		}
		return
	}
	t.Fatal("FILE_WRITE_BYTES must remain registered to report the permission refusal")
}

func TestBinaryWriteWithoutWorkspaceRemainsUnsupported(t *testing.T) {
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{FilesDisabled: true})
	if anyHandles(handlers, JobTypeFileWriteBytes) {
		t.Fatal("a missing workspace must not be misreported as a permission refusal")
	}
}

func TestEnabledNode_BinaryWriteStillWritesBytes(t *testing.T) {
	workspace := t.TempDir()
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{WorkspaceDir: workspace})
	for _, handler := range handlers {
		if !handler.CanHandle(JobTypeFileWriteBytes) {
			continue
		}
		result, err := handler.Execute(context.Background(), &Job{
			ID: "enabled-write", Type: JobTypeFileWriteBytes,
			Payload: map[string]any{"path": "allowed.bin", "content": "YWJj"},
		}, &NoOpStreamWriter{})
		if err != nil || result == nil || result.Status != JobStatusSuccess {
			t.Fatalf("enabled binary write failed: result=%+v err=%v", result, err)
		}
		data, err := os.ReadFile(filepath.Join(workspace, "allowed.bin"))
		if err != nil || string(data) != "abc" {
			t.Fatalf("written bytes=%q err=%v", data, err)
		}
		return
	}
	t.Fatal("enabled FILE_WRITE_BYTES handler missing")
}
