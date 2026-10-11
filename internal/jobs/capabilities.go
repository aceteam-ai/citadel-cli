// internal/jobs/capabilities.go
//
// Generic node job lifecycle contract (aceteam#10876 / citadel-cli#1287). The
// node job plumbing built for FILE_INDEX — the heavy execution lane, the
// cooperative mid-job cancel watch, the long watchdog tier, the open progress
// vocabulary, and signed work receipts — used to be turned on by runner-side
// type ALLOWLISTS (internal/worker/deadline.go's heavyLaneJobTypes /
// cooperativeCancelJobTypes, each holding only FILE_INDEX, plus a per-type branch
// in resolveJobTimeout). Every new adopter (FINETUNE, RUN_JOB_TEMPLATE,
// LLM_INFERENCE, ...) had to edit the runner.
//
// This makes the lifecycle a CONTRACT a handler DECLARES. A handler optionally
// implements CapabilityDeclarer; the runner READS the declaration instead of
// keeping allowlists. FILE_INDEX is the first adopter and its declared values
// reproduce its prior runner-allowlist behavior exactly.
package jobs

// Lane selects the runner execution lane a job type runs on. The zero value
// (LaneNormal) preserves the existing inline / semaphore-pool / serialized /
// GPU-inference / long-session routing the runner already applies by job type;
// only LaneHeavy opts a handler onto the dedicated heavy lane (exec 1, admit 2).
type Lane int

const (
	// LaneNormal is the default: the runner routes by its existing static sets
	// (serializedLaneJobTypes, gpuBoundJobTypes, longSessionJobTypes) and the
	// inline/semaphore fallbacks. A handler that declares nothing gets this.
	LaneNormal Lane = iota
	// LaneHeavy routes the job onto the dedicated heavy execution lane (exec 1,
	// admit 2): a single DISPATCHED long writer off the fetch loop, isolated from
	// the manifest-writer serialized lane. FILE_INDEX declares this.
	LaneHeavy
)

// DeadlineTier selects the fallback per-job watchdog budget. The zero value
// (DeadlineUnset) means "no declaration": the runner falls back to its existing
// job-type maps (unboundedJobTypes / longSessionJobTypes / RUN_JOB_TEMPLATE /
// default). An explicit tier wins over those maps.
type DeadlineTier int

const (
	// DeadlineUnset defers to the runner's existing per-type fallback resolution.
	DeadlineUnset DeadlineTier = iota
	// DeadlineDefault is the ordinary tier (WORKER_JOB_TIMEOUT_SECONDS, 60min).
	DeadlineDefault
	// DeadlineLong is the long-session/heavy tier (WORKER_JOB_TIMEOUT_LONG_SECONDS,
	// 4h). FILE_INDEX declares this (a full-archive walk+embed runs for hours).
	DeadlineLong
	// DeadlineUnbounded applies no fallback watchdog (0 = unbounded).
	DeadlineUnbounded
)

// ReceiptOwner records WHO signs the dispatched-work receipt for a job type, so
// the runner never double-signs and never fails closed for work it does not own.
type ReceiptOwner int

const (
	// ReceiptOwnerNone is the default: the runner neither signs a work receipt
	// nor fails closed for this job type. Every handler that registers no work
	// action gets this — SHELL_COMMAND, FILE_READ, etc. never fail on a keyless
	// node (the critical fail-closed scope, aceteam#10876 C7 posture preserved).
	ReceiptOwnerNone ReceiptOwner = iota
	// ReceiptOwnerHandler means the HANDLER produces its own signed work receipt
	// (via jobs.WorkReceiptConfig.attachWorkRecord) and owns the fail-closed
	// posture. FILE_INDEX and embedding declare this; the runner touches nothing
	// (their behavior stays byte-identical).
	ReceiptOwnerHandler
	// ReceiptOwnerRunner means the RUNNER signs a work receipt for every
	// dispatched success of this job type (default on, independent of
	// CITADEL_SIGN_AEP_RECEIPTS) and fails the job closed when signing is
	// unavailable. LLM_INFERENCE declares this.
	ReceiptOwnerRunner
)

// JobCapabilities is the lifecycle contract a handler DECLARES for the job type
// it serves (aceteam#10876 / citadel-cli#1287). The zero value is the inert
// default: normal lane, deferred deadline, no cooperative cancel, no stages, no
// work action — i.e. exactly how every non-declaring handler behaves today.
type JobCapabilities struct {
	// CooperativeCancel requests the runner's graceful mid-job cancel watch: it
	// polls job:cancelled:<id> and sets JobContext.CancelRequested at a safe unit
	// boundary (NOT a hard ctx cancel). FILE_INDEX declares this to commit the
	// current file then stop with partial counts.
	CooperativeCancel bool
	// Lane selects the execution lane (LaneNormal | LaneHeavy).
	Lane Lane
	// Deadline selects the fallback watchdog tier (DeadlineUnset defers to the
	// runner's maps).
	Deadline DeadlineTier
	// Stages is the OPEN per-handler progress vocabulary (replacing the closed
	// ProgressStage enum). Informational: the throttled progress path treats a
	// stage as an opaque string, so this documents what a handler can emit.
	Stages []string
	// WorkActions are the aep work actions this job type registers. A non-empty
	// list means the job is METERED dispatched work. Combined with ReceiptOwner,
	// it drives signing: ReceiptOwnerRunner + a work action => the runner signs
	// and fails closed; ReceiptOwnerHandler => the handler already signs.
	WorkActions []string
	// ReceiptOwner records who signs the work receipt (see ReceiptOwner).
	ReceiptOwner ReceiptOwner
}

// CapabilityDeclarer is optionally implemented by a single-purpose jobs.JobHandler
// to declare its lifecycle capabilities. The worker adapter maps the handler's
// registered job type onto this declaration; the runner reads it in place of the
// old allowlists. A handler that does not implement it gets the zero-value
// defaults (identical to pre-#1287 behavior).
type CapabilityDeclarer interface {
	Capabilities() JobCapabilities
}
