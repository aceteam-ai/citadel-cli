// internal/jobs/cobrowse_session.go
//
// Interactive browser-session job handler (issue #793). Bridges the node's job stream
// to the multi-session CobrowseSessionManager so a caller can start, query, and stop
// isolated, concurrently-running browser sessions on the node.
//
// A single COBROWSE_SESSION job type carries an "action" payload field (start /
// status / stop / reset / navigate / screenshot / click / type / extract / handoff /
// resume, issue #978 added the last six), keeping the worker registration to one
// entry -- mirroring the single-session COBROWSE handler's wire shape. Each job is one
// short lifecycle or scripted-interaction action; the browser session itself is a
// long-lived process owned by the manager, not by any one job. The handler returns a
// JSON document as its output bytes so the backend can parse a structured result.
//
// The six #978 actions are all addressed by a required "session_id" field (unlike the
// old singleton COBROWSE job, which has exactly one implicit session) and are refused
// with the session's driver-arbitration error (platform.ErrHandedOff, surfaced
// verbatim) while a human is attached to that session -- see
// platform/cobrowse_session_actions.go for the full arbitration contract.
package jobs

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowseprofile"
	"github.com/aceteam-ai/citadel-cli/internal/cobrowsestream"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/services"
)

// Session lifecycle action types carried in the job payload's "action" field.
const (
	CobrowseSessionActionStart  = "start"
	CobrowseSessionActionStatus = "status"
	CobrowseSessionActionStop   = "stop"
	// CobrowseSessionActionReset discards the encrypted persistent profile named
	// in the "profile" field. It requires NO pin: under the node's no-recovery
	// master-PIN model a forgotten PIN can never unlock the profile again, so
	// reset is the only escape hatch and must work without it.
	CobrowseSessionActionReset = "reset"

	// Session-scoped CDP scripting actions (issue #978). Each requires
	// "session_id"; see startSession's PIN handling note -- none of these read
	// or touch "pin" at all, since they operate on an already-running session.
	CobrowseSessionActionNavigate   = "navigate"
	CobrowseSessionActionScreenshot = "screenshot"
	CobrowseSessionActionClick      = "click"
	CobrowseSessionActionType       = "type"
	CobrowseSessionActionExtract    = "extract"
	// Driver arbitration actions (issue #978): hand a session's scripting
	// control to the human, or return it to the agent.
	CobrowseSessionActionHandoff = "handoff"
	CobrowseSessionActionResume  = "resume"
)

// CobrowseSessionHandler handles COBROWSE_SESSION jobs by delegating to the node's
// CobrowseSessionManager singleton.
type CobrowseSessionHandler struct{}

// NewCobrowseSessionHandler constructs an interactive browser-session handler.
func NewCobrowseSessionHandler() *CobrowseSessionHandler { return &CobrowseSessionHandler{} }

func (h *CobrowseSessionHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	action := job.Payload["action"]
	if action == "" {
		return nil, fmt.Errorf("job payload missing 'action' field")
	}
	// Log the action and profile name only; the "pin" field is a secret and is
	// never logged here or passed anywhere but nodevault's Unlock.
	ctx.Log("info", "     - [Job %s] browser-session action: %s", job.ID, action)

	mgr := platform.GetCobrowseSessionManager()

	switch action {
	case CobrowseSessionActionStart:
		return startSession(mgr, job)

	case CobrowseSessionActionStatus:
		// With a session_id, report that one session. Without, list every session --
		// the queryable-state contract, always answerable even with none running.
		id := job.Payload["session_id"]
		if id == "" {
			return sessionListResult(mgr.List())
		}
		st, ok := mgr.SessionStatus(id)
		if !ok {
			return nil, fmt.Errorf("no such browser session: %q", id)
		}
		return sessionStatusResult(st)

	case CobrowseSessionActionStop:
		id := job.Payload["session_id"]
		if id == "" {
			return nil, fmt.Errorf("stop requires a 'session_id' field")
		}
		if err := mgr.Stop(id); err != nil {
			return nil, err
		}
		out, _ := json.Marshal(map[string]any{"stopped": id})
		return out, nil

	case CobrowseSessionActionReset:
		name := job.Payload["profile"]
		if name == "" {
			return nil, fmt.Errorf("reset requires a 'profile' field")
		}
		if err := cobrowseprofile.Reset(network.GetNodeConfigDir(), name); err != nil {
			return nil, err
		}
		out, _ := json.Marshal(map[string]any{"reset": name})
		return out, nil

	case CobrowseSessionActionNavigate:
		id := job.Payload["session_id"]
		if id == "" {
			return nil, fmt.Errorf("navigate requires a 'session_id' field")
		}
		url := job.Payload["url"]
		if url == "" {
			return nil, fmt.Errorf("navigate requires a 'url' field")
		}
		st, err := mgr.Navigate(id, url)
		if err != nil {
			return nil, err
		}
		return sessionStatusResult(st)

	case CobrowseSessionActionScreenshot:
		id := job.Payload["session_id"]
		if id == "" {
			return nil, fmt.Errorf("screenshot requires a 'session_id' field")
		}
		img, err := mgr.Screenshot(id)
		if err != nil {
			return nil, err
		}
		out, _ := json.Marshal(map[string]any{
			"image":  img,
			"format": "png",
		})
		return out, nil

	case CobrowseSessionActionClick:
		id := job.Payload["session_id"]
		if id == "" {
			return nil, fmt.Errorf("click requires a 'session_id' field")
		}
		selector := job.Payload["selector"]
		x, y, hasCoords, err := parseClickCoords(job.Payload)
		if err != nil {
			return nil, err
		}
		if selector == "" && !hasCoords {
			return nil, fmt.Errorf("click requires a 'selector' field or both 'x' and 'y' fields")
		}
		st, err := mgr.Click(id, selector, x, y)
		if err != nil {
			return nil, err
		}
		return sessionStatusResult(st)

	case CobrowseSessionActionType:
		id := job.Payload["session_id"]
		if id == "" {
			return nil, fmt.Errorf("type requires a 'session_id' field")
		}
		text := job.Payload["text"]
		if text == "" {
			return nil, fmt.Errorf("type requires a 'text' field")
		}
		st, err := mgr.Type(id, text)
		if err != nil {
			return nil, err
		}
		return sessionStatusResult(st)

	case CobrowseSessionActionExtract:
		id := job.Payload["session_id"]
		if id == "" {
			return nil, fmt.Errorf("extract requires a 'session_id' field")
		}
		selector := job.Payload["selector"]
		if selector == "" {
			return nil, fmt.Errorf("extract requires a 'selector' field")
		}
		result, err := mgr.Extract(id, selector, parseAttrNames(job.Payload["attrs"]))
		if err != nil {
			return nil, err
		}
		out, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return out, nil

	case CobrowseSessionActionHandoff:
		id := job.Payload["session_id"]
		if id == "" {
			return nil, fmt.Errorf("handoff requires a 'session_id' field")
		}
		st, err := mgr.Handoff(id)
		if err != nil {
			return nil, err
		}
		return sessionStatusResult(st)

	case CobrowseSessionActionResume:
		id := job.Payload["session_id"]
		if id == "" {
			return nil, fmt.Errorf("resume requires a 'session_id' field")
		}
		st, err := mgr.Resume(id)
		if err != nil {
			return nil, err
		}
		return sessionStatusResult(st)

	default:
		return nil, fmt.Errorf("unknown browser-session action: %q", action)
	}
}

// parseClickCoords reads the optional "x"/"y" payload fields as an explicit
// viewport coordinate. Both must be present and numeric for hasCoords to be
// true; a payload with neither field simply reports hasCoords=false (the
// caller falls back to the selector form), while a payload with one field set
// and not the other, or a non-numeric value, is a clear input error rather
// than a silently-ignored partial coordinate.
func parseClickCoords(payload map[string]string) (x, y *float64, hasCoords bool, err error) {
	xStr, hasX := payload["x"]
	yStr, hasY := payload["y"]
	if !hasX && !hasY {
		return nil, nil, false, nil
	}
	if !hasX || !hasY {
		return nil, nil, false, fmt.Errorf("click requires both 'x' and 'y' when either is given")
	}
	xf, xerr := strconv.ParseFloat(xStr, 64)
	if xerr != nil {
		return nil, nil, false, fmt.Errorf("click 'x' is not a number: %w", xerr)
	}
	yf, yerr := strconv.ParseFloat(yStr, 64)
	if yerr != nil {
		return nil, nil, false, fmt.Errorf("click 'y' is not a number: %w", yerr)
	}
	return &xf, &yf, true, nil
}

// parseAttrNames splits the extract action's optional "attrs" field (a
// comma-separated attribute name list, e.g. "href,id") into a slice, trimming
// whitespace and dropping empty entries. An empty/absent field yields no
// requested attributes (extract still returns "text").
func parseAttrNames(raw string) []string {
	if raw == "" {
		return nil
	}
	var attrs []string
	for _, a := range strings.Split(raw, ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			attrs = append(attrs, a)
		}
	}
	return attrs
}

// startSession launches one browser session. When the payload carries a "profile"
// name it launches with the encrypted, PIN-unlocked persistent profile of that
// name; the "pin" field is REQUIRED in that case and unlocks the shared node vault
// at use time. Absent/wrong PIN fails closed (no session, no plaintext profile) —
// a present "profile" NEVER silently falls back to a throwaway session, since that
// would look like "logged out" and break the persistence contract. With no
// "profile" field it launches a throwaway session exactly as before.
func startSession(mgr *platform.CobrowseSessionManager, job *nexus.Job) ([]byte, error) {
	name := job.Payload["profile"]
	if name == "" {
		st, err := mgr.StartSession(job.Payload["url"])
		if err != nil {
			return nil, err
		}
		return sessionStatusResult(st)
	}

	pin := job.Payload["pin"]
	if pin == "" {
		return nil, fmt.Errorf("a 'pin' is required to open persistent profile %q", name)
	}
	baseDir := network.GetNodeConfigDir()
	handle, err := cobrowseprofile.OpenHandle(baseDir, name, pin, nodevault.Open(baseDir))
	if err != nil {
		// Wrong/absent PIN, lockout, unconfigured vault, busy profile, or bad name:
		// all fail closed, no session and no plaintext profile materialized.
		return nil, err
	}
	// Ownership of handle transfers to the manager, which guarantees Close on every
	// path (including a failed start), so there is no Close to do here.
	st, err := mgr.StartSessionWithProfile(job.Payload["url"], handle)
	if err != nil {
		return nil, err
	}
	return sessionStatusResult(st)
}

// sessionStreamInfo is the additive stream-endpoint hint carried on start/status
// results so the live viewer (#8132) does not have to hardcode the port/path
// convention. A viewer reaches the session over the mesh at
// ws://<node_vpn_ip>:<port><path>?id=<session_id>.
type sessionStreamInfo struct {
	Port int    `json:"port"`
	Path string `json:"path"`
}

// sessionResult is one session's status plus the stream endpoint. The embedded
// status promotes its fields to the top level, so existing fields are unchanged
// and "stream" is purely additive.
type sessionResult struct {
	platform.CobrowseSessionStatus
	Stream sessionStreamInfo `json:"stream"`
}

// sessionStatusResult marshals one session's status (plus stream endpoint) to
// JSON output bytes.
func sessionStatusResult(st platform.CobrowseSessionStatus) ([]byte, error) {
	out, err := json.Marshal(sessionResult{
		CobrowseSessionStatus: st,
		Stream: sessionStreamInfo{
			Port: services.CobrowseStreamPort,
			Path: cobrowsestream.StreamPath,
		},
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// sessionListResult marshals a list of session statuses to JSON output bytes, always
// as a JSON array (never null) so a caller can iterate an empty result unconditionally.
func sessionListResult(list []platform.CobrowseSessionStatus) ([]byte, error) {
	if list == nil {
		list = []platform.CobrowseSessionStatus{}
	}
	out, err := json.Marshal(map[string]any{"sessions": list})
	if err != nil {
		return nil, err
	}
	return out, nil
}
