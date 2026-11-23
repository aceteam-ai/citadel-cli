package worker

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

// A PC connected to AceTeam from its local setup page runs co-browse only
// (citadel-cli#1867 C5). Its permissions file then carries a setup posture,
// and the worker refuses the job types below: each one starts, installs,
// exposes or builds software on the PC, which the Services and Provision
// permissions (HTTP routes only) do not cover. The refusal is terminal on the
// first delivery, like every other gate refusal.

// ReasonSetupPosture is the refusal reason for a job refused by the posture.
const ReasonSetupPosture = "setup_posture"

const setupPostureRefusalMessage = "this PC was connected to AceTeam from its setup page and runs co-browse only; " +
	"it does not start, install, expose or build software for remote jobs"

// setupPostureRefusedJobTypes is the set the posture refuses.
// TestSetupPostureRefusedJobTypes pins it.
var setupPostureRefusedJobTypes = map[string]struct{}{
	JobTypeServiceStart:      {},
	JobTypeServiceStop:       {},
	JobTypeAppDeploy:         {},
	JobTypeAppStart:          {},
	JobTypeAppStop:           {},
	JobTypeAppDestroy:        {},
	JobTypeInstanceProvision: {},
	JobTypeInstanceStart:     {},
	JobTypeInstanceStop:      {},
	JobTypeInstanceDestroy:   {},
	JobTypeModuleSet:         {},
	JobTypeExposeSet:         {},
	JobTypeWhatsAppProvision: {},
	JobTypeAndroidBuild:      {},
	JobTypeGomobileBuild:     {},
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
	if _, gated := setupPostureRefusedJobTypes[jobType]; !gated {
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
