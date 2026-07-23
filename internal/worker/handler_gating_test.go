package worker

import "testing"

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
// NOT register the screen/VNC or file-browse handlers, so those fabric jobs are
// refused ("unsupported job type") rather than silently executed.
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
		JobTypeFileWriteBytes, JobTypeFileEdit, JobTypeFileList,
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
