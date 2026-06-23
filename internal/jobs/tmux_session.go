// internal/jobs/tmux_session.go
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/tmux"
)

// TmuxSessionHandler manages named tmux sessions on the node via the standard
// job-dispatch mechanism (issue #302). It lets the app create/ensure/list the
// persistent sessions that the terminal WebSocket later attaches to.
//
// The handler never assumes tmux is installed: when no usable tmux binary can
// be resolved it returns a clear, actionable error rather than crashing.
//
// Actions (job payload "action"):
//
//	"ensure" (default) -- create the named session if absent (idempotent). The
//	    session runs a plain shell; "claude" is never launched here.
//	"create"           -- alias for "ensure".
//	"list"             -- return the names of all sessions on the node.
//	"has"              -- report whether the named session exists.
//
// Payload fields:
//
//	"action"  -- one of the actions above (optional; defaults to "ensure").
//	"name"    -- session name (required for ensure/create/has). Validated.
//	"shell"   -- shell to run inside a new session (optional).
type TmuxSessionHandler struct {
	// DefaultShell is used for "ensure"/"create" when the payload omits "shell".
	// Empty lets tmux use its configured default.
	DefaultShell string
}

// NewTmuxSessionHandler constructs a handler with an optional default shell.
func NewTmuxSessionHandler(defaultShell string) *TmuxSessionHandler {
	return &TmuxSessionHandler{DefaultShell: defaultShell}
}

// tmuxSessionResult is the JSON envelope returned to the dispatcher.
type tmuxSessionResult struct {
	Action   string   `json:"action"`
	Name     string   `json:"name,omitempty"`
	Exists   bool     `json:"exists,omitempty"`
	Sessions []string `json:"sessions,omitempty"`
	Message  string   `json:"message,omitempty"`
}

func (h *TmuxSessionHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	action := strings.ToLower(strings.TrimSpace(job.Payload["action"]))
	if action == "" {
		action = "ensure"
	}

	mgr, err := tmux.NewManager()
	if err != nil {
		// tmux not available: surface the actionable guidance from the package.
		return nil, err
	}

	bg := context.Background()

	switch action {
	case "list":
		sessions, err := mgr.ListSessions(bg)
		if err != nil {
			return nil, err
		}
		ctx.Log("info", "     - [Job %s] tmux list-sessions: %d session(s)", job.ID, len(sessions))
		return marshalResult(tmuxSessionResult{Action: "list", Sessions: sessions})

	case "has":
		name := job.Payload["name"]
		exists, err := mgr.HasSession(bg, name)
		if err != nil {
			return nil, err
		}
		return marshalResult(tmuxSessionResult{Action: "has", Name: name, Exists: exists})

	case "ensure", "create":
		name := job.Payload["name"]
		shell := job.Payload["shell"]
		if shell == "" {
			shell = h.DefaultShell
		}
		if err := mgr.EnsureSession(bg, name, shell); err != nil {
			return nil, err
		}
		ctx.Log("info", "     - [Job %s] ensured tmux session %q", job.ID, name)
		return marshalResult(tmuxSessionResult{
			Action:  "ensure",
			Name:    name,
			Exists:  true,
			Message: fmt.Sprintf("session %q is ready; attach via the terminal WebSocket", name),
		})

	default:
		return nil, fmt.Errorf("unsupported tmux session action %q (want ensure|create|list|has)", action)
	}
}

func marshalResult(r tmuxSessionResult) ([]byte, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal tmux session result: %w", err)
	}
	return data, nil
}
