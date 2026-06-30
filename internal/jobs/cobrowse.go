// internal/jobs/cobrowse.go
//
// Co-browse job handler (issue #4079). Bridges the node's Redis job stream to
// the persistent CobrowseManager so the AI agent can drive a managed headed
// Chromium over CDP and hand control to a human for login / 2FA.
//
// Each job is one short action against the long-lived browser owned by
// platform.GetCobrowseManager(); the browser itself persists between jobs.
// The handler returns a JSON document as its output bytes so the backend MCP
// tool can parse a structured result (mirroring the VNC handlers' wire shape).
package jobs

import (
	"encoding/json"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// Co-browse job action types. A single COBROWSE job type carries an "action"
// payload field, keeping the worker registration to one entry while supporting
// all six MCP tools.
const (
	CobrowseActionStart      = "start"
	CobrowseActionNavigate   = "navigate"
	CobrowseActionHandoff    = "handoff"
	CobrowseActionResume     = "resume"
	CobrowseActionStatus     = "status"
	CobrowseActionScreenshot = "screenshot"
)

// CobrowseHandler handles COBROWSE jobs by delegating to the node's
// CobrowseManager singleton.
type CobrowseHandler struct{}

// NewCobrowseHandler constructs a co-browse handler.
func NewCobrowseHandler() *CobrowseHandler { return &CobrowseHandler{} }

func (h *CobrowseHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	action := job.Payload["action"]
	if action == "" {
		return nil, fmt.Errorf("job payload missing 'action' field")
	}
	ctx.Log("info", "     - [Job %s] co-browse action: %s", job.ID, action)

	mgr := platform.GetCobrowseManager()

	switch action {
	case CobrowseActionStart:
		st, err := mgr.Start(job.Payload["profile"], job.Payload["url"], 0)
		return statusResult(st, err)

	case CobrowseActionNavigate:
		url := job.Payload["url"]
		if url == "" {
			return nil, fmt.Errorf("navigate requires a 'url' field")
		}
		st, err := mgr.Navigate(url)
		return statusResult(st, err)

	case CobrowseActionHandoff:
		st, err := mgr.Handoff()
		return statusResult(st, err)

	case CobrowseActionResume:
		st, err := mgr.Resume()
		return statusResult(st, err)

	case CobrowseActionStatus:
		return statusResult(mgr.Status(), nil)

	case CobrowseActionScreenshot:
		img, err := mgr.Screenshot()
		if err != nil {
			return nil, err
		}
		st := mgr.Status()
		out, _ := json.Marshal(map[string]any{
			"image":  img,
			"format": "png",
			"driver": string(st.Driver),
			"url":    st.URL,
		})
		return out, nil

	default:
		return nil, fmt.Errorf("unknown co-browse action: %q", action)
	}
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
