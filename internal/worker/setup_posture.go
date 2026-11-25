package worker

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

// A PC connected to AceTeam from its local setup page runs co-browse only
// (citadel-cli#1867 C5). Its permissions file then carries a setup posture,
// and the worker runs only the job types in setupPostureAllowedJobTypes;
// every other type, including any added later, gets a terminal
// ReasonSetupPosture refusal on its first delivery, like every other gate
// refusal. TestSetupPostureClassifiesEveryJobType makes a new job type fail
// until it is classified.

// ReasonSetupPosture is the refusal reason for a job refused by the posture.
const ReasonSetupPosture = "setup_posture"

const setupPostureRefusalMessage = "this PC was connected to AceTeam from its setup page and runs co-browse only; " +
	"this kind of job is not run here"

// setupPostureAllowedJobTypes is the whole set a setup-page PC runs. Each
// entry needs a reason; keep it this small.
var setupPostureAllowedJobTypes = map[string]struct{}{
	// The point of the posture: node-wide co-browse sessions and their
	// actions. They run behind the public-only egress guard, which the
	// posture turns on. stage_file and set_file_input recheck the live Files
	// permission, which only the human at the PC can turn on. The legacy
	// COBROWSE job (one unguarded browser) is not listed.
	JobTypeCobrowseSession: {},

	// Keeping the node manageable. APPLY_DEVICE_CONFIG goes through
	// jobs.applySetupPostureFilter, which drops every request that turns
	// something on, so it can only tighten. AGENT_UPDATE installs only a
	// release whose checksums verify against the compiled release key. It is
	// how a security fix reaches this PC. WORKER_CONTROL restarts this
	// worker and runs nothing else.
	JobTypeApplyDeviceConfig: {},
	JobTypeAgentUpdate:       {},
	JobTypeWorkerControl:     {},

	// The human-granted surfaces. Each handler rechecks its own live
	// permission per job, and both default to off on this PC: SHELL_COMMAND
	// the Shell permission plus the node passcode, the FILE_* types the Files
	// permission ("read and upload files in the workspace"). Only the setup
	// page, the viewer or `citadel permission` on the PC can turn them on.
	JobTypeShellCommand:       {},
	JobTypeFileRead:           {},
	JobTypeFileReadBytes:      {},
	JobTypeFileReadRangeV1:    {},
	JobTypeFileReadRangeV2:    {},
	JobTypeFileWrite:          {},
	JobTypeFileWriteBytes:     {},
	JobTypeFileFetch:          {},
	JobTypeFileEdit:           {},
	JobTypeFileList:           {},
	JobTypeFileSearch:         {},
	JobTypeFileIndex:          {},
	JobTypeFileSemanticSearch: {},
}

var setupPostureSource atomic.Pointer[func() bool]

// SetSetupPostureSource installs the live posture reader. cmd installs it in
// an init(), so both worker entry points (citadel work and the control center)
// read it at every job. Nil (tests, other embedders) refuses nothing.
func SetSetupPostureSource(f func() bool) {
	if f == nil {
		setupPostureSource.Store(nil)
		return
	}
	setupPostureSource.Store(&f)
}

// setupPostureRefusal returns the refusal for jobType, or nil.
func setupPostureRefusal(jobType string) error {
	if _, allowed := setupPostureAllowedJobTypes[jobType]; allowed {
		return nil
	}
	src := setupPostureSource.Load()
	if src == nil || !(*src)() {
		return nil
	}
	return &jobs.ShellRefusal{Reason: ReasonSetupPosture, Message: setupPostureRefusalMessage}
}

// refusingHandler answers one job with a fixed refusal.
type refusingHandler struct{ err error }

func (h refusingHandler) CanHandle(string) bool { return true }

func (h refusingHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	return &JobResult{Status: JobStatusFailure, Error: h.err, Duration: time.Duration(0)}, h.err
}
