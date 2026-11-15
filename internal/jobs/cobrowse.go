// internal/jobs/cobrowse.go
//
// Legacy co-browse job handler (issue #4079, retired for remote actions in
// #1672). Status and stop remain available to observe and clean up the known
// singleton. All start/action jobs refuse before manager discovery and point
// callers at guarded COBROWSE_SESSION instead.
package jobs

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// Co-browse job action types. A single COBROWSE job type carries an "action"
// payload field. Old action names remain wire constants but now refuse; only
// status and stop are accepted on this legacy job family.
const (
	CobrowseActionStart      = "start"
	CobrowseActionStop       = "stop"
	CobrowseActionNavigate   = "navigate"
	CobrowseActionHandoff    = "handoff"
	CobrowseActionResume     = "resume"
	CobrowseActionStatus     = "status"
	CobrowseActionScreenshot = "screenshot"
)

// ErrLegacyCobrowseDisabled refuses the unguarded legacy dispatch path. Callers
// must explicitly start and address a guarded session; no browser is retargeted.
var ErrLegacyCobrowseDisabled = errors.New("legacy COBROWSE start/actions refused; start a new guarded COBROWSE_SESSION via cobrowse_start and use its session_id")

type legacyCobrowseControl interface {
	Status() platform.CobrowseStatus
	Stop() error
}

// CobrowseHandler retains only observation and cleanup for legacy COBROWSE jobs.
// The optional factory is a test seam: refusals happen before manager discovery.
type CobrowseHandler struct {
	manager func() legacyCobrowseControl
}

// NewCobrowseHandler constructs the legacy observation/cleanup handler.
func NewCobrowseHandler() *CobrowseHandler { return &CobrowseHandler{} }

func (h *CobrowseHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	action := job.Payload["action"]
	if action == "" {
		return nil, fmt.Errorf("job payload missing 'action' field")
	}
	ctx.Log("info", "     - [Job %s] co-browse action: %s", job.ID, action)
	if action != CobrowseActionStatus && action != CobrowseActionStop {
		return nil, ErrLegacyCobrowseDisabled
	}
	var mgr legacyCobrowseControl
	if h.manager != nil {
		mgr = h.manager()
	} else {
		mgr = platform.GetCobrowseManager()
	}
	if action == CobrowseActionStop {
		if err := mgr.Stop(); err != nil {
			return nil, err
		}
	}
	return statusResult(mgr.Status(), nil)
}

// statusResult marshals a CobrowseStatus to JSON output bytes, propagating any
// error from the manager (e.g. ErrHandedOff / ErrNotStarted) so the worker
// reports FAILURE and the backend surfaces the reason verbatim.
func statusResult(st platform.CobrowseStatus, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	out, mErr := json.Marshal(map[string]any{
		"running":    st.Running,
		"driver":     string(st.Driver),
		"url":        st.URL,
		"debug_port": st.DebugPort,
		"profile":    st.Profile,
		"display":    st.Display,
		"started_at": st.StartedAt,
	})
	if mErr != nil {
		return nil, mErr
	}
	return out, nil
}
