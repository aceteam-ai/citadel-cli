// internal/jobs/cobrowse_session.go
//
// Interactive browser-session job handler (issue #793). Bridges the node's job stream
// to the multi-session CobrowseSessionManager so a caller can start, query, and stop
// isolated, concurrently-running browser sessions on the node.
//
// A single COBROWSE_SESSION job type carries an "action" payload field (start /
// status / stop), keeping the worker registration to one entry -- mirroring the
// single-session COBROWSE handler's wire shape. Each job is one short lifecycle
// action; the browser session itself is a long-lived process owned by the manager,
// not by any one job. The handler returns a JSON document as its output bytes so the
// backend can parse a structured result.
package jobs

import (
	"encoding/json"
	"fmt"

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

	default:
		return nil, fmt.Errorf("unknown browser-session action: %q", action)
	}
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
