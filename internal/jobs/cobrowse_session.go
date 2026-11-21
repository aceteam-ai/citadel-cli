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
// after explicit human handoff. Recording also refuses a passive viewer and
// cancels on viewer attachment, Stop or handoff. See
// platform/cobrowse_session_actions.go for the full arbitration contract.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowseprofile"
	"github.com/aceteam-ai/citadel-cli/internal/cobrowsestream"
	"github.com/aceteam-ai/citadel-cli/internal/jobprivacy"
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
	CobrowseSessionActionRecord     = "record"
	CobrowseSessionActionClick      = "click"
	CobrowseSessionActionType       = "type"
	CobrowseSessionActionSelect     = "select"
	CobrowseSessionActionKey        = "key"
	CobrowseSessionActionScroll     = "scroll"
	CobrowseSessionActionExtract    = "extract"
	// Driver arbitration actions (issue #978): hand a session's scripting
	// control to the human, or return it to the agent.
	CobrowseSessionActionHandoff = "handoff"
	// File upload (citadel-cli#1721): stage bytes into the session's private
	// staging dir, then fill a file input with one staged file.
	CobrowseSessionActionStageFile    = "stage_file"
	CobrowseSessionActionSetFileInput = "set_file_input"
	CobrowseSessionActionResume       = "resume"
)

// CobrowseStageBytesKey is the stage_file payload field carrying the file's
// bytes (standard base64). It is a retained-copy secret: see jobprivacy.BytesKey.
const CobrowseStageBytesKey = jobprivacy.BytesKey

// CobrowseSessionHandler handles COBROWSE_SESSION jobs by delegating to the node's
// CobrowseSessionManager singleton.
type CobrowseSessionHandler struct {
	WorkspaceDir string
	manager      *platform.CobrowseSessionManager
	// Enabled gates record, stage_file and set_file_input on the node's Files
	// permission, read live per job (as FILE_WRITE_BYTES does). Unset denies:
	// a handler built without a permission source never writes a user file.
	// Other session actions are not affected.
	Enabled func() bool
}

func (h *CobrowseSessionHandler) filesAllowed() bool {
	return h.Enabled != nil && h.Enabled()
}

// NewCobrowseSessionHandler constructs an interactive browser-session handler.
func NewCobrowseSessionHandler() *CobrowseSessionHandler {
	return &CobrowseSessionHandler{}
}

// NewCobrowseSessionHandlerWithManager uses an isolated session manager. Production
// uses NewCobrowseSessionHandler and the node-wide tracked manager.
func NewCobrowseSessionHandlerWithManager(m *platform.CobrowseSessionManager) *CobrowseSessionHandler {
	return &CobrowseSessionHandler{manager: m}
}

func (h *CobrowseSessionHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	mgr := h.manager
	if mgr == nil {
		mgr = platform.GetCobrowseSessionManager()
	}
	action, id, target := job.Payload["action"], job.Payload["session_id"], job.Payload["target_id"]
	tracked := id != "" && mgr.SessionTargetsEnabled(id)
	var out []byte
	var err error
	run := func() error { out, err = h.execute(ctx, job, mgr); return err }
	switch action {
	case CobrowseSessionActionNavigate, CobrowseSessionActionClick, CobrowseSessionActionType, CobrowseSessionActionSelect, CobrowseSessionActionKey, CobrowseSessionActionScroll, CobrowseSessionActionHandoff, CobrowseSessionActionResume:
		if id != "" && (tracked || target != "") {
			err = mgr.WithTargetAssertion(id, target, run)
		} else {
			err = run()
		}
	case CobrowseSessionActionScreenshot, CobrowseSessionActionExtract:
		if id != "" && tracked {
			err = mgr.WithTargetAssertion(id, "", run)
		} else {
			err = run()
		}
	default:
		if target != "" {
			return nil, fmt.Errorf("target_id is not supported for %s", action)
		}
		err = run()
	}
	if err != nil {
		return nil, err
	}
	// Decorate every successful session action, including non-status read results.
	// Reset and list/capabilities are not addressed to a browser page.
	if id != "" && (action == CobrowseSessionActionExtract || action == CobrowseSessionActionSelect) {
		st, ok := mgr.SessionStatus(id)
		if !ok && tracked {
			return nil, platform.ErrNotStarted
		}
		if ok {
			return decorateTargetResult(out, st)
		}
	}
	return out, nil
}

func decorateTargetResult(out []byte, st platform.CobrowseSessionStatus) ([]byte, error) {
	if len(st.Features) == 0 {
		return out, nil
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, err
	}
	result["active_target"] = st.ActiveTarget
	targets := st.Targets
	if targets == nil {
		targets = []platform.CobrowseTarget{}
	}
	result["targets"], result["open_targets"], result["features"] = targets, len(targets), st.Features
	if st.Interstitial != "" {
		result["interstitial"] = st.Interstitial
	}
	return json.Marshal(result)
}

func (h *CobrowseSessionHandler) execute(ctx JobContext, job *nexus.Job, mgr *platform.CobrowseSessionManager) ([]byte, error) {
	action := job.Payload["action"]
	if action == "" {
		return nil, fmt.Errorf("job payload missing 'action' field")
	}
	// Log the action and profile name only; the "pin" field is a secret and is
	// never logged here or passed anywhere but nodevault's Unlock.
	ctx.Log("info", "     - [Job %s] browser-session action: %s", job.ID, action)

	switch action {
	case "capabilities":
		features := []string{}
		if mgr.TargetsEnabled() {
			features = append(features, "tabs")
		}
		return json.Marshal(map[string]any{"features": features})
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
			if e, ended := mgr.EndedSession(id); ended {
				return json.Marshal(map[string]any{
					"id": id, "state": "ended", "ended_reason": e.Reason,
					"detail": e.Detail, "ended_at": e.At,
				})
			}
			return nil, fmt.Errorf("no such browser session: %q", id)
		}
		return sessionStatusResult(mgr.WithPageSignals(st))

	case CobrowseSessionActionStop:
		id := job.Payload["session_id"]
		if id == "" {
			return nil, fmt.Errorf("stop requires a 'session_id' field")
		}
		st, _ := mgr.SessionStatus(id)
		if err := mgr.Stop(id); err != nil {
			return nil, err
		}
		out, _ := json.Marshal(map[string]any{"stopped": id})
		st.ActiveTarget = ""
		st.Targets = nil
		st.OpenTargets = 0
		return decorateTargetResult(out, st)

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
		var img string
		var st platform.CobrowseSessionStatus
		var page platform.CobrowseTarget
		var err error
		if !mgr.SessionTargetsEnabled(id) && job.Payload["target_id"] == "" {
			// Preserve main's single-connection screenshot and status read for
			// legacy sessions; tracked reads also carry the selected page.
			img, st, err = mgr.ScreenshotStatus(id)
		} else {
			img, page, err = mgr.ScreenshotTarget(id, job.Payload["target_id"])
			if err == nil {
				var ok bool
				st, ok = mgr.SessionStatus(id)
				if !ok {
					return nil, platform.ErrNotStarted
				}
			}
		}
		if err != nil {
			return nil, err
		}
		return sessionScreenshotResult(img, st, page)

	case CobrowseSessionActionRecord:
		if !h.filesAllowed() {
			return nil, ErrFilesDisabled
		}
		id := job.Payload["session_id"]
		if id == "" {
			return nil, fmt.Errorf("record requires a 'session_id' field")
		}
		seconds, err := strconv.Atoi(job.Payload["seconds"])
		if err != nil || seconds < 1 || seconds > 30 {
			return nil, fmt.Errorf("record requires 'seconds' from 1 to 30")
		}
		if h.WorkspaceDir == "" {
			return nil, fmt.Errorf("record requires a configured workspace")
		}
		return mgr.WithRecordingPort(id, ctx.Context(), func(recordCtx context.Context, port int, commit func(func() error) error) ([]byte, error) {
			return saveCobrowseRecordingWithCommit(h.WorkspaceDir, seconds, func(output *os.File) error {
				return cobrowsestream.RecordWebM(recordCtx, port, output, time.Duration(seconds)*time.Second)
			}, commit)
		})

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
			return actionFailureResult(err, job.Payload["report_partial"] == "true")
		}
		return sessionStatusResult(st)

	case CobrowseSessionActionType:
		id, text, opts, err := parseTypeAction(job.Payload)
		if err != nil {
			return nil, err
		}
		st, err := mgr.TypeWith(id, text, opts)
		if err != nil {
			return actionFailureResult(err, job.Payload["report_partial"] == "true")
		}
		return sessionStatusResult(st)

	case CobrowseSessionActionSelect:
		id, selector := job.Payload["session_id"], job.Payload["selector"]
		value, label := job.Payload["value"], job.Payload["label"]
		if id == "" || selector == "" || (value == "" && label == "") || (value != "" && label != "") {
			return nil, fmt.Errorf("select requires session_id, selector, and exactly one of value or label")
		}
		selected, err := mgr.Select(id, selector, value, label)
		if err != nil {
			return nil, err
		}
		return json.Marshal(selected)

	case CobrowseSessionActionKey:
		id, key, modifiers, err := parseKeyAction(job.Payload)
		if err != nil {
			return nil, err
		}
		st, err := mgr.KeyWith(id, key, modifiers)
		if err != nil {
			return actionFailureResult(err, job.Payload["report_partial"] == "true")
		}
		return sessionStatusResult(st)

	case CobrowseSessionActionScroll:
		id := job.Payload["session_id"]
		if id == "" {
			return nil, fmt.Errorf("scroll requires session_id")
		}
		dx, err := strconv.ParseFloat(job.Payload["dx"], 64)
		if err != nil {
			return nil, fmt.Errorf("scroll requires numeric dx: %w", err)
		}
		dy, err := strconv.ParseFloat(job.Payload["dy"], 64)
		if err != nil {
			return nil, fmt.Errorf("scroll requires numeric dy: %w", err)
		}
		x, y, hasCoords, err := parseClickCoords(job.Payload)
		if err != nil {
			return nil, err
		}
		if job.Payload["selector"] == "" && !hasCoords {
			return nil, fmt.Errorf("scroll requires selector or both x and y")
		}
		st, err := mgr.Scroll(id, job.Payload["selector"], x, y, dx, dy)
		if err != nil {
			return actionFailureResult(err, job.Payload["report_partial"] == "true")
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
		result, page, err := mgr.ExtractTarget(id, job.Payload["target_id"], selector, parseAttrNames(job.Payload["attrs"]))
		if err != nil {
			return nil, err
		}
		out, err := json.Marshal(result)
		if err == nil && page.ID != "" {
			var fields map[string]any
			if err = json.Unmarshal(out, &fields); err == nil {
				fields["target_id"] = page.ID
				out, err = json.Marshal(fields)
			}
		}
		if err != nil {
			return nil, err
		}
		return out, nil

	case CobrowseSessionActionStageFile:
		if !h.filesAllowed() {
			return nil, ErrFilesDisabled
		}
		id, name := job.Payload["session_id"], job.Payload["name"]
		if id == "" || name == "" {
			return nil, fmt.Errorf("stage_file requires session_id and name")
		}
		if err := platform.ValidateStagedName(name); err != nil {
			return nil, err
		}
		// Size is checked on the encoded form first so an oversized payload is
		// refused before it is decoded into a second copy.
		encoded := job.Payload[CobrowseStageBytesKey]
		if base64.StdEncoding.DecodedLen(len(encoded)) > platform.MaxCobrowseStagedFileBytes+2 {
			return nil, platform.ErrStagedTooLarge
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("stage_file %s is not valid base64", CobrowseStageBytesKey)
		}
		staged, err := mgr.StageFile(id, name, data)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{
			"session_id": id, "name": staged.Name, "size": staged.Size, "sha256": staged.SHA256,
		})

	case CobrowseSessionActionSetFileInput:
		if !h.filesAllowed() {
			return nil, ErrFilesDisabled
		}
		id, selector, staged := job.Payload["session_id"], job.Payload["selector"], job.Payload["staged_name"]
		if id == "" || staged == "" {
			return nil, fmt.Errorf("set_file_input requires session_id and staged_name")
		}
		expected := job.Payload["expected_url"]
		if expected == "" {
			return nil, fmt.Errorf("set_file_input requires 'expected_url'")
		}
		var nodeID int
		if raw := job.Payload["backend_node_id"]; raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("set_file_input 'backend_node_id' is not a positive integer")
			}
			nodeID = n
		}
		if (selector == "") == (nodeID == 0) {
			return nil, fmt.Errorf("set_file_input requires exactly one of 'selector' or 'backend_node_id'")
		}
		st, err := mgr.SetFileInput(id, selector, nodeID, staged, expected)
		if err != nil {
			return actionFailureResult(err, job.Payload["report_partial"] == "true")
		}
		return sessionStatusResult(st)

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

// saveCobrowseRecording keeps creation, publication and cleanup rooted in the
// workspace. The encoder receives an open file, so swapping captures after
// creation cannot redirect its bytes to an outside path.
func saveCobrowseRecording(workspace string, seconds int, capture func(*os.File) error) ([]byte, error) {
	return saveCobrowseRecordingWithCommit(workspace, seconds, capture, func(publish func() error) error {
		return publish()
	})
}

// The commit callback atomically checks session privacy state and publishes
// under the session lock. A cancelled recording leaves only a temporary file,
// which the deferred cleanup removes before Stop or Handoff returns.
func saveCobrowseRecordingWithCommit(workspace string, seconds int, capture func(*os.File) error, commit func(func() error) error) ([]byte, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, fmt.Errorf("open recording workspace: %w", err)
	}
	defer root.Close()
	if err := root.MkdirAll("captures", 0o700); err != nil {
		return nil, fmt.Errorf("create capture directory: %w", err)
	}
	dirInfo, err := root.Lstat("captures")
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("capture directory must be a real directory")
	}
	captureRoot, err := root.OpenRoot("captures")
	if err != nil {
		return nil, fmt.Errorf("open capture directory: %w", err)
	}
	defer captureRoot.Close()
	stableDir, err := captureRoot.Stat(".")
	if err != nil || !os.SameFile(dirInfo, stableDir) {
		return nil, fmt.Errorf("capture directory changed before recording")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("create recording name: %w", err)
	}
	name := "cobrowse-" + hex.EncodeToString(nonce[:]) + ".webm"
	tempName := "." + name
	tempRel := filepath.Join("captures", tempName)
	finalRel := filepath.Join("captures", name)
	output, err := captureRoot.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create recording file: %w", err)
	}
	defer output.Close()
	defer captureRoot.Remove(tempName)
	if err := capture(output); err != nil {
		return nil, err
	}
	info, err := output.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect recording: %w", err)
	}
	if info.Size() == 0 || info.Size() > cobrowsestream.MaxRecordBytes {
		return nil, fmt.Errorf("recording is empty or exceeds the 24 MiB export limit")
	}
	if err := output.Sync(); err != nil {
		return nil, fmt.Errorf("flush recording: %w", err)
	}
	currentDir, err := root.Lstat("captures")
	if err != nil || !os.SameFile(dirInfo, currentDir) {
		return nil, fmt.Errorf("capture directory changed during recording")
	}
	currentTemp, err := root.Lstat(tempRel)
	if err != nil || !os.SameFile(info, currentTemp) {
		return nil, fmt.Errorf("recording file changed during capture")
	}
	var out []byte
	if err := commit(func() error {
		// Link creates the final name only when it is absent. Rename would
		// replace an existing file, including one raced in by another process.
		if err := root.Link(tempRel, finalRel); err != nil {
			return fmt.Errorf("publish recording: %w", err)
		}
		finalInfo, err := root.Lstat(finalRel)
		if err != nil || !os.SameFile(info, finalInfo) {
			_ = root.Remove(finalRel)
			return fmt.Errorf("published recording changed before receipt")
		}
		out, _ = json.Marshal(map[string]any{
			"path": filepath.ToSlash(finalRel), "format": "webm",
			"bytes": info.Size(), "duration_s": seconds,
		})
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
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
		return sessionStatusResult(mgr.WithPageSignals(st))
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
		if errors.Is(err, nodevault.ErrWeakSecret) {
			// #820: a short access PIN never encrypts data at rest. On this node
			// the master PIN is the profile's only key, so it must be a passphrase.
			return nil, fmt.Errorf("persistent profile %q needs a passphrase, not a short PIN, because it is encrypted on disk: "+
				"rotate the node master PIN to a passphrase of four or more random words with 'citadel passcode rotate' at the node "+
				"(this also changes the secret for remote access to the node): %w", name, err)
		}
		return nil, err
	}
	// Ownership of handle transfers to the manager, which guarantees Close on every
	// path (including a failed start), so there is no Close to do here.
	st, err := mgr.StartSessionWithProfile(job.Payload["url"], handle)
	if err != nil {
		return nil, err
	}
	return sessionStatusResult(mgr.WithPageSignals(st))
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

// sessionScreenshotResult keeps the legacy screenshot fields plus the typed
// browser-interstitial signal, including on untracked sessions.
func sessionScreenshotResult(img string, st platform.CobrowseSessionStatus, page platform.CobrowseTarget) ([]byte, error) {
	pageURL := st.URL
	if page.ID != "" {
		pageURL = page.URL
	}
	fields := map[string]any{"image": img, "format": "png", "driver": string(st.Driver), "url": pageURL}
	if page.ID != "" {
		fields["target_id"] = page.ID
	}
	if st.Interstitial != "" {
		fields["interstitial"] = st.Interstitial
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return decorateTargetResult(out, st)
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
	return decorateTargetResult(out, st)
}

// CobrowseReportPartialKey is the payload flag with which a caller says it
// understands a structured partial-effect result. Without it an incomplete
// action is an error whose message says what happened, because platform tools
// treat any non-error result as success and would audit "clicked".
const CobrowseReportPartialKey = "report_partial"

// actionFailureResult handles an action that hit its deadline or failed after
// it may have acted. Without reportPartial it stays an error (the
// ActionIncompleteError summary: what was scrolled, focused, typed, and that it
// was not retried). With it, the result is a structured done=false status
// carrying the effects, additive to every existing field. A failure that
// changed nothing, and every refusal, is returned unchanged.
func actionFailureResult(err error, reportPartial bool) ([]byte, error) {
	ae, ok := platform.AsActionIncomplete(err)
	if !ok || !ae.Reportable() || !reportPartial {
		return nil, err
	}
	out, merr := sessionStatusResult(ae.Status)
	if merr != nil {
		return nil, err
	}
	var m map[string]any
	if json.Unmarshal(out, &m) != nil {
		return nil, err
	}
	m["done"] = false
	m["timed_out"] = ae.TimedOut
	m["error"] = ae.Error()
	return json.Marshal(m)
}

// sessionListResult marshals a list of session statuses to JSON output bytes, always
// as a JSON array (never null) so a caller can iterate an empty result unconditionally.
func sessionListResult(list []platform.CobrowseSessionStatus) ([]byte, error) {
	sessions := make([]json.RawMessage, 0, len(list))
	for _, st := range list {
		raw, err := json.Marshal(st)
		if err != nil {
			return nil, err
		}
		out, err := decorateTargetResult(raw, st)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, out)
	}
	return json.Marshal(map[string]any{"sessions": sessions})
}

// parseTypeAction validates a type payload: session_id, text, and the optional
// selector and clear (citadel-cli#1730). Empty text is allowed only with clear.
// Refusals name the field, never its value.
func parseTypeAction(payload map[string]string) (string, string, platform.TypeOptions, error) {
	var opts platform.TypeOptions
	id := payload["session_id"]
	if id == "" {
		return "", "", opts, fmt.Errorf("type requires a 'session_id' field")
	}
	switch payload["clear"] {
	case "", "false":
	case "true":
		opts.Clear = true
	default:
		return "", "", opts, fmt.Errorf("type 'clear' must be true or false")
	}
	opts.Selector = payload["selector"]
	if len(opts.Selector) > platform.MaxCobrowseSelectorLen {
		return "", "", opts, platform.ErrSelectorTooLong
	}
	text := payload["text"]
	if text == "" && !opts.Clear {
		return "", "", opts, fmt.Errorf("type requires a 'text' field (empty text is allowed only with clear)")
	}
	return id, text, opts, nil
}

// parseKeyAction validates a key payload: session_id, key, and the optional
// modifiers bitmask, a base-10 integer 0 to 15, limited to the
// platform.CheckKeyChord allowlist (anything else is refused).
func parseKeyAction(payload map[string]string) (string, string, int, error) {
	id, key := payload["session_id"], payload["key"]
	if id == "" || key == "" {
		return "", "", 0, fmt.Errorf("key requires session_id and key")
	}
	mods := 0
	if raw, ok := payload["modifiers"]; ok && raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 15 || strconv.Itoa(n) != raw {
			return "", "", 0, platform.ErrModifiersOutRange
		}
		mods = n
	}
	if err := platform.CheckKeyChord(key, mods); err != nil {
		return "", "", 0, err
	}
	return id, key, mods, nil
}
