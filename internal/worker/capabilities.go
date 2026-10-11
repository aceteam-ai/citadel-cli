package worker

import "github.com/aceteam-ai/citadel-cli/internal/jobs"

// capabilityDeclarer is optionally implemented by a worker.JobHandler to declare
// the generic lifecycle capabilities the runner honors for a job type
// (aceteam#10876 / citadel-cli#1287). It REPLACES the per-type allowlists that
// used to live in deadline.go (heavyLaneJobTypes / cooperativeCancelJobTypes) and
// the resolveJobTimeout heavy-lane branch.
//
// It is deliberately PERMISSION-INDEPENDENT: DeclaredCapabilities reports the
// static declaration for jobType even when the handler is currently gated off
// (e.g. a FILE_INDEX adapter whose `files` permission is toggled off). This
// matches the old allowlists exactly — lane/cancel/deadline routing never
// depended on the live permission gate; only actual dispatch (findHandler, via
// CanHandle) does.
type capabilityDeclarer interface {
	// DeclaredCapabilities returns the capabilities for jobType and whether this
	// handler OWNS that job type's declaration. ok=false means "not mine".
	DeclaredCapabilities(jobType string) (jobs.JobCapabilities, bool)
}

// capabilitiesOf reads the declared capabilities for jobType directly from a
// handler already in hand (executeJob / executeWithDeadline have one). A handler
// that does not declare, or does not own jobType, yields the zero value.
func capabilitiesOf(h JobHandler, jobType string) jobs.JobCapabilities {
	if d, ok := h.(capabilityDeclarer); ok {
		if caps, owns := d.DeclaredCapabilities(jobType); owns {
			return caps
		}
	}
	return jobs.JobCapabilities{}
}

// capabilitiesFor returns the declared lifecycle capabilities for jobType. It
// scans handlers for the first that owns a declaration; when none does it returns
// the zero value (LaneNormal, DeadlineUnset, no cooperative cancel, no work
// action), which reproduces pre-#1287 behavior for every non-declaring handler.
// Used where no handler is in hand yet (the Run dispatch switch, resolveJobTimeout,
// the cooperative-cancel watch).
func (r *Runner) capabilitiesFor(jobType string) jobs.JobCapabilities {
	for _, h := range r.handlers {
		if caps := capabilitiesOf(h, jobType); capsDeclared(h, jobType) {
			return caps
		}
	}
	return jobs.JobCapabilities{}
}

// capsDeclared reports whether h owns a declaration for jobType (so a handler
// that explicitly declares the zero value is still distinguished from one that
// declares nothing).
func capsDeclared(h JobHandler, jobType string) bool {
	if d, ok := h.(capabilityDeclarer); ok {
		_, owns := d.DeclaredCapabilities(jobType)
		return owns
	}
	return false
}

// findHandler returns the first registered handler that can process jobType, or
// nil. It is the shared handler-lookup used by executeJob (dispatch) — it honors
// CanHandle's live permission gate, unlike capabilitiesFor's static declaration
// scan.
func (r *Runner) findHandler(jobType string) JobHandler {
	for _, h := range r.handlers {
		if h.CanHandle(jobType) {
			return h
		}
	}
	return nil
}
