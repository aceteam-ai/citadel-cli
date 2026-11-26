package worker

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

// wantSetupPostureAllowed pins the allowlist. Changing it is a security
// decision: give the new entry a reason in setupPostureAllowedJobTypes.
var wantSetupPostureAllowed = []string{
	JobTypeCobrowseSession,
	JobTypeApplyDeviceConfig, JobTypeAgentUpdate, JobTypeWorkerControl,
	JobTypeShellCommand,
	JobTypeFileRead, JobTypeFileReadBytes, JobTypeFileReadRangeV1, JobTypeFileReadRangeV2,
	JobTypeFileWrite, JobTypeFileWriteIfUnchanged, JobTypeFileWriteBytes, JobTypeFileFetch, JobTypeFileEdit,
	JobTypeFileList, JobTypeFileSearch, JobTypeFileIndex, JobTypeFileSemanticSearch,
}

// wantSetupPostureRefused is every other job type this build knows, refused on
// a setup-page PC. A job type added later is refused by default, and this
// test fails until it is listed here or, with a reason, in the allowlist.
var wantSetupPostureRefused = []string{
	JobTypeTmuxSession, JobTypeDownloadModel, JobTypeOllamaPull,
	JobTypeLlamaCppInference, JobTypeVLLMInference, JobTypeOllamaInference, JobTypeLLMInference, JobTypeEmbedding,
	JobTypeExtraction, JobTypeHTTPProxy, JobTypeWebFetch,
	JobTypeServiceStart, JobTypeServiceStop, JobTypeServiceStatus,
	JobTypeSandboxSuspend, JobTypeSandboxResume, JobTypeModelCachePull, JobTypeModelCacheEvict,
	JobTypeIOSBuild, JobTypeAndroidBuild, JobTypeGomobileBuild,
	JobTypeFileScreenshot, JobTypeVNCScreenshot, JobTypeVNCType, JobTypeVNCKeys, JobTypeVNCActions,
	JobTypeCobrowse, JobTypeTranscribeAudio, JobTypeSynthesizeSpeech, JobTypeMediaGenerate,
	JobTypeWhatsAppProvision, JobTypeResourceSnapshot, JobTypeInstanceMessage, JobTypeModuleSet,
	JobTypeFineTuneStart, JobTypeExposeSet, JobTypeExposeList, JobTypeLaneInventory, JobTypeLaneOperation, JobTypeUnexpose,
	JobTypeMeetingJoin, JobTypeHuddleJoin, JobTypeHuddleTeardown, JobTypeDocumentRasterize,
	JobTypeShowPairingCode, JobTypeClearPairingCode, JobTypeVoiceManage,
	JobTypeAppDeploy, JobTypeAppStop, JobTypeAppStart, JobTypeAppStatus, JobTypeAppLogs, JobTypeAppDestroy,
	JobTypeRunJobTemplate,
	JobTypeInstanceProvision, JobTypeInstanceStart, JobTypeInstanceStop, JobTypeInstanceDestroy, JobTypeInstanceStatus,
	// Registered outside this package (internal/workflow imports worker).
	"WORKFLOW_RUN",
}

// postureHandlers is every handler this package registers, built with every
// optional directory set so the conditional registrations are included.
func postureHandlers(t *testing.T) []JobHandler {
	t.Helper()
	return CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
		WorkspaceDir: t.TempDir(), ConfigDir: t.TempDir(), PermissionsDir: t.TempDir(), IndexesDir: t.TempDir(),
		FilesEnabled: func() bool { return true }, ShellEnabled: func() bool { return true },
	})
}

// TestSetupPostureClassifiesEveryJobType: the allowlist is exactly as
// pinned, every job type this build knows (and every type a registered
// handler answers) is classified, and nothing is in both lists.
func TestSetupPostureClassifiesEveryJobType(t *testing.T) {
	var got []string
	for k := range setupPostureAllowedJobTypes {
		got = append(got, k)
	}
	sort.Strings(got)
	want := append([]string(nil), wantSetupPostureAllowed...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("allowlist %v, want %v", got, want)
	}
	classified := map[string]string{}
	for _, jt := range wantSetupPostureAllowed {
		classified[jt] = "allowed"
	}
	for _, jt := range wantSetupPostureRefused {
		if classified[jt] != "" {
			t.Errorf("%s is both allowed and refused", jt)
		}
		classified[jt] = "refused"
	}
	for _, jt := range allKnownJobTypes {
		if classified[jt] == "" {
			t.Errorf("job type %s is not classified for setup-page PCs: add it to wantSetupPostureRefused, or with a reason to setupPostureAllowedJobTypes", jt)
		}
	}
	// Every handler registered here answers only classified types.
	for _, h := range postureHandlers(t) {
		for _, jt := range allKnownJobTypes {
			if h.CanHandle(jt) && classified[jt] == "" {
				t.Errorf("registered handler for unclassified %s", jt)
			}
		}
	}
}

// recordingHandler counts executions of every job type it is offered.
type recordingHandler struct{ runs int }

func (h *recordingHandler) CanHandle(string) bool { return true }
func (h *recordingHandler) Execute(context.Context, *Job, StreamWriter) (*JobResult, error) {
	h.runs++
	return &JobResult{Status: JobStatusSuccess}, nil
}

// TestSetupPostureRefusesEverythingOutsideTheAllowlist: with the posture on,
// every refused type never reaches its handler and fails once, terminally,
// with the typed reason; every allowed type and every type on a node without
// the posture runs as before.
func TestSetupPostureRefusesEverythingOutsideTheAllowlist(t *testing.T) {
	on := true
	SetSetupPostureSource(func() bool { return on })
	t.Cleanup(func() { SetSetupPostureSource(nil) })

	run := func(jobType string) (*recordingHandler, *MockJobSource, *MockStreamWriter, bool) {
		h := &recordingHandler{}
		source := NewMockJobSource("test", nil)
		runner := NewRunner(source, []JobHandler{h}, RunnerConfig{WorkerID: "test-worker"})
		stream := &MockStreamWriter{}
		ok := runner.executeJob(context.Background(), &Job{ID: "j-" + jobType, Type: jobType, Metadata: JobMetadata{Attempts: 1, MaxAttempts: 3}}, stream, time.Now(), false, 0)
		return h, source, stream, ok
	}

	for _, jt := range append(append([]string(nil), wantSetupPostureRefused...), "SOME_FUTURE_JOB") {
		h, source, stream, ok := run(jt)
		if ok || h.runs != 0 {
			t.Errorf("%s ran on a posture PC: ok=%v runs=%d", jt, ok, h.runs)
			continue
		}
		if stream.errorCount != 1 || stream.erroredRecover || len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 {
			t.Errorf("%s: want one terminal error and a Fail: errors=%d failed=%d nacked=%d", jt, stream.errorCount, len(source.FailedJobs()), len(source.NackedJobs()))
			continue
		}
		var refusal struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(stream.erroredErr.Error()), &refusal); err != nil || refusal.Reason != ReasonSetupPosture {
			t.Errorf("%s: refusal reason %q (%v)", jt, refusal.Reason, err)
		}
	}
	for _, jt := range wantSetupPostureAllowed {
		if h, _, _, ok := run(jt); !ok || h.runs != 1 {
			t.Errorf("allowed %s must still run on a posture PC: ok=%v runs=%d", jt, ok, h.runs)
		}
	}
	on = false
	for _, jt := range []string{JobTypeServiceStart, JobTypeDownloadModel, JobTypeTmuxSession} {
		if h, _, _, ok := run(jt); !ok || h.runs != 1 {
			t.Errorf("a node without the posture must run %s: ok=%v runs=%d", jt, ok, h.runs)
		}
	}
}
