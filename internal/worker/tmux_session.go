package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// TmuxSessionConfig wires TMUX_SESSION. Every field except LogFn is required;
// a missing one leaves the type unadvertised and every job refused.
type TmuxSessionConfig struct {
	OrgID, NodeID string
	// Gate is the live shell permission plus node passcode, the same gate
	// SHELL_COMMAND uses. Enabled, State and Throttle must all be set.
	Gate jobs.ShellGate
	// Sessions performs the tmux action. Production uses
	// jobs.NewTmuxSessionHandler, which owns name and lease validation.
	Sessions jobs.JobHandler
	LogFn    func(level, msg string)
}

// TmuxSessionHandler serves TMUX_SESSION. Order is load-bearing: the exact
// own-node stream and node id first (so a misrouted job never touches the
// passcode throttle), then the shell gate for every action except list and
// has, then tmux.
type TmuxSessionHandler struct{ cfg TmuxSessionConfig }

func NewTmuxSessionHandler(cfg TmuxSessionConfig) *TmuxSessionHandler {
	return &TmuxSessionHandler{cfg: cfg}
}

func (h *TmuxSessionHandler) wired() bool {
	c := h.cfg
	return inventoryQueueID.MatchString(c.OrgID) && inventoryQueueID.MatchString(c.NodeID) &&
		c.Gate.Enabled != nil && c.Gate.State != nil && c.Gate.Throttle != nil && c.Sessions != nil
}

// CanHandle advertises TMUX_SESSION only when the gates are wired and the live
// shell permission is on.
func (h *TmuxSessionHandler) CanHandle(kind string) bool {
	return kind == JobTypeTmuxSession && h.wired() && h.cfg.Gate.IsEnabled()
}

// RefusalFor explains an unadvertised TMUX_SESSION without executing it.
func (h *TmuxSessionHandler) RefusalFor(jobType string) *jobs.ShellRefusal {
	if jobType != JobTypeTmuxSession {
		return nil
	}
	if !h.wired() {
		return tmuxSessionRefusal("capability_unavailable")
	}
	if !h.cfg.Gate.IsEnabled() {
		return jobs.WorkflowShellRefusal(jobs.ReasonShellDisabled)
	}
	return nil
}

func tmuxSessionRefusal(reason string) *jobs.ShellRefusal {
	return &jobs.ShellRefusal{Reason: reason, Message: "TMUX_SESSION refused: " + reason}
}

// tmuxSessionUngated are the read-only actions that need only the own stream
// and the live shell permission. Every other action, including the empty
// default, passes the full shell gate.
var tmuxSessionUngated = map[string]bool{"list": true, "has": true}

func (h *TmuxSessionHandler) Execute(ctx context.Context, job *Job, _ StreamWriter) (*JobResult, error) {
	start := time.Now()
	refuse := func(err *jobs.ShellRefusal) (*JobResult, error) {
		if h.cfg.LogFn != nil && job != nil {
			h.cfg.LogFn("warn", fmt.Sprintf("Refusing %s job %s: %s", JobTypeTmuxSession, job.ID, err.Reason))
		}
		return &JobResult{Status: JobStatusFailure, Error: err, Duration: time.Since(start)}, err
	}
	if job == nil || job.Type != JobTypeTmuxSession || !h.wired() {
		return refuse(tmuxSessionRefusal("capability_unavailable"))
	}
	// Only the Redis adapters bind SourceQueue to the stream actually read.
	if (job.Source != "redis" && job.Source != "redis-api") || job.SourceQueue != "jobs:v1:shell:org_"+h.cfg.OrgID+":node:"+h.cfg.NodeID {
		return refuse(tmuxSessionRefusal("stream_mismatch"))
	}
	if node, ok := job.Payload["node_id"].(string); !ok || node != h.cfg.NodeID {
		return refuse(tmuxSessionRefusal("node_mismatch"))
	}
	if target, present := job.Payload["target_node"]; present && target != h.cfg.NodeID {
		return refuse(tmuxSessionRefusal("node_mismatch"))
	}

	payload := make(map[string]string, len(job.Payload))
	for key, value := range job.Payload {
		if value == nil {
			continue
		}
		s, ok := value.(string)
		if !ok {
			return refuse(tmuxSessionRefusal("invalid_request"))
		}
		payload[key] = s
	}
	passcode := payload[jobs.ShellPasscodePayloadKey]
	// The passcode authorizes this job and is never tmux input.
	delete(payload, jobs.ShellPasscodePayloadKey)

	// Normalized exactly as jobs.TmuxSessionHandler reads it.
	action := strings.ToLower(strings.TrimSpace(payload["action"]))
	if action == "" {
		action = "ensure"
	}
	if tmuxSessionUngated[action] {
		if !h.cfg.Gate.IsEnabled() {
			return refuse(jobs.WorkflowShellRefusal(jobs.ReasonShellDisabled))
		}
	} else {
		if reason := jobs.ShellGateReason(h.cfg.Gate, passcode); reason != "" {
			return refuse(&jobs.ShellRefusal{Reason: reason, Message: "TMUX_SESSION refused by the node shell gate: " + reason})
		}
		if action != "ensure" && action != "create" {
			return refuse(tmuxSessionRefusal("invalid_request"))
		}
	}

	output, err := h.cfg.Sessions.Execute(jobs.JobContext{LogFn: h.cfg.LogFn, Ctx: ctx},
		&nexus.Job{ID: job.ID, Type: job.Type, SourceQueue: job.SourceQueue, Payload: payload})
	duration := time.Since(start)
	if err != nil {
		return &JobResult{Status: JobStatusFailure, Error: err, Duration: duration,
			Output: map[string]any{"error": err.Error(), "output": string(output)}}, err
	}
	return &JobResult{Status: JobStatusSuccess, Duration: duration, Output: map[string]any{"output": string(output)}}, nil
}

var _ JobHandler = (*TmuxSessionHandler)(nil)
