package worker

import (
	"context"
	"strconv"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/laneop"
)

// LaneOperationConfig wires LANE_OPERATION (#1659). Every field is required;
// a missing one leaves the type unadvertised and every job refused.
type LaneOperationConfig struct {
	OrgID, NodeID string
	// Gate is the live shell permission plus node passcode, the same gate
	// SHELL_COMMAND uses. Enabled, State and Throttle must all be set.
	Gate    jobs.ShellGate
	Service *laneop.Service
	// Available reports whether tmux can serve lane operations here. It is
	// consulted on every advertisement, so production caches it.
	Available func() bool
}

// LaneOperationHandler serves the aceteam lane_capture and lane_send wire
// contract. Order is load-bearing: exact own-node stream and node id first (so
// a misrouted job never touches the passcode throttle), then the shell gate,
// then the strict payload, then tmux.
type LaneOperationHandler struct{ cfg LaneOperationConfig }

func NewLaneOperationHandler(cfg LaneOperationConfig) *LaneOperationHandler {
	return &LaneOperationHandler{cfg: cfg}
}

func (h *LaneOperationHandler) wired() bool {
	c := h.cfg
	return inventoryQueueID.MatchString(c.OrgID) && inventoryQueueID.MatchString(c.NodeID) &&
		c.Gate.Enabled != nil && c.Gate.State != nil && c.Gate.Throttle != nil &&
		c.Service != nil && c.Service.Exec != nil && c.Service.Ledger != nil &&
		c.Available != nil && c.Available()
}

// CanHandle advertises LANE_OPERATION only when tmux is usable, the gates are
// wired and the live shell permission is on.
func (h *LaneOperationHandler) CanHandle(kind string) bool {
	return kind == JobTypeLaneOperation && h.wired() && h.cfg.Gate.IsEnabled()
}

// RefusalFor explains an unadvertised LANE_OPERATION without executing it.
func (h *LaneOperationHandler) RefusalFor(jobType string) *jobs.ShellRefusal {
	if jobType != JobTypeLaneOperation {
		return nil
	}
	if !h.wired() {
		return &jobs.ShellRefusal{Reason: "capability_unavailable", Message: "LANE_OPERATION needs tmux 3.2 or newer on a Unix node, run as the lane's own user"}
	}
	if !h.cfg.Gate.IsEnabled() {
		return jobs.WorkflowShellRefusal(jobs.ReasonShellDisabled)
	}
	return nil
}

func laneRefusal(reason string) *jobs.ShellRefusal {
	return &jobs.ShellRefusal{Reason: reason, Message: "LANE_OPERATION refused: " + reason}
}

// laneWireKeys are the payload keys aceteam's LaneJobEnvelope sends, plus the
// dispatcher's transport metadata. Anything else is refused.
var laneWireKeys = map[string]bool{
	"operation": true, "lane": true, "lease_id": true, "node_id": true, "pane": true,
	"max_bytes": true, "message": true, "offer_id": true, jobs.ShellPasscodePayloadKey: true,
	"target_node": true, "timeout_ms": true, "rayId": true,
}

func (h *LaneOperationHandler) Execute(ctx context.Context, job *Job, _ StreamWriter) (*JobResult, error) {
	if job == nil || job.Type != JobTypeLaneOperation || !h.wired() {
		return nil, laneRefusal("capability_unavailable")
	}
	// Only the Redis adapters bind SourceQueue to the stream actually read,
	// the same proof LANE_INVENTORY requires.
	if (job.Source != "redis" && job.Source != "redis-api") || job.SourceQueue != "jobs:v1:shell:org_"+h.cfg.OrgID+":node:"+h.cfg.NodeID {
		return nil, laneRefusal("stream_mismatch")
	}
	str := func(key string) (string, bool) {
		v, ok := job.Payload[key]
		if !ok {
			return "", false
		}
		s, ok := v.(string)
		return s, ok
	}
	if node, ok := str("node_id"); !ok || node != h.cfg.NodeID {
		return nil, laneRefusal("node_mismatch")
	}
	if target, present := job.Payload["target_node"]; present && target != h.cfg.NodeID {
		return nil, laneRefusal("node_mismatch")
	}
	operation, _ := str("operation")
	passcode, _ := str(jobs.ShellPasscodePayloadKey)
	rejectSend := func(reason string) (*JobResult, error) {
		offer, _ := str("offer_id")
		if !laneop.OfferPattern.MatchString(offer) {
			offer = ""
		}
		return &JobResult{Status: JobStatusSuccess, Output: laneop.SendResult{Status: laneop.StatusRejected, Reason: reason}.Map(offer)}, nil
	}
	if reason := jobs.ShellGateReason(h.cfg.Gate, passcode); reason != "" {
		if operation == "send" {
			// A send result is read only from a successful end event; an
			// error would leave the offer submission_unknown forever.
			return rejectSend(reason)
		}
		return nil, &jobs.ShellRefusal{Reason: reason, Message: "LANE_OPERATION refused by the node shell gate: " + reason}
	}
	for key := range job.Payload {
		if !laneWireKeys[key] {
			if operation == "send" {
				return rejectSend("invalid_request")
			}
			return nil, laneRefusal("invalid_request")
		}
	}
	lane, _ := str("lane")
	lease, _ := str("lease_id")
	pane, _ := str("pane")
	if !laneop.LanePattern.MatchString(lane) || !laneop.PanePattern.MatchString(pane) || !validLease(lease) {
		if operation == "send" {
			return rejectSend("invalid_request")
		}
		return nil, laneRefusal("invalid_request")
	}
	switch operation {
	case "capture":
		raw, _ := str("max_bytes")
		limit, err := strconv.Atoi(raw)
		if err != nil || strconv.Itoa(limit) != raw || limit <= 0 || limit > laneop.MaxCaptureBytes {
			return nil, laneRefusal("invalid_bound")
		}
		if _, present := job.Payload["message"]; present {
			return nil, laneRefusal("invalid_request")
		}
		result, err := h.cfg.Service.Capture(ctx, pane, limit, passcode)
		if err != nil {
			reason := "tmux_unavailable"
			if refusal, ok := err.(*laneop.Refusal); ok {
				reason = refusal.Reason
			}
			return nil, laneRefusal(reason)
		}
		return &JobResult{Status: JobStatusSuccess, Output: result.Map()}, nil
	case "send":
		message, okMessage := str("message")
		offer, _ := str("offer_id")
		if _, present := job.Payload["max_bytes"]; present || !okMessage || !laneop.ValidMessage(message) || !laneop.OfferPattern.MatchString(offer) {
			return rejectSend("invalid_request")
		}
		result := h.cfg.Service.Send(ctx, laneop.SendRequest{Lane: lane, LeaseID: lease, NodeID: h.cfg.NodeID, Pane: pane, OfferID: offer, Message: message})
		return &JobResult{Status: JobStatusSuccess, Output: result.Map(offer)}, nil
	default:
		return nil, laneRefusal("invalid_request")
	}
}

func validLease(lease string) bool {
	if lease == "" || len(lease) > 128 {
		return false
	}
	for _, r := range lease {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}
