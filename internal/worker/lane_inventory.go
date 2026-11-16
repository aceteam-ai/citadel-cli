package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/aceteam-ai/citadel-cli/internal/laneinventory"
)

type LaneInventoryConfig struct {
	OrgID, NodeID string
	Collect       func(context.Context) (laneinventory.Report, error)
	// Nil is default-off. Rechecked for both advertisement and execution.
	Enabled func() bool
}

type LaneInventoryHandler struct{ cfg LaneInventoryConfig }

func NewLaneInventoryHandler(cfg LaneInventoryConfig) *LaneInventoryHandler {
	return &LaneInventoryHandler{cfg: cfg}
}
func (h *LaneInventoryHandler) CanHandle(kind string) bool {
	return kind == JobTypeLaneInventory && h.cfg.Enabled != nil && h.cfg.Enabled()
}

var ErrLaneInventoryDisabled = errors.New("LANE_INVENTORY is disabled on this node")

var inventoryQueueID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (h *LaneInventoryHandler) Execute(ctx context.Context, job *Job, _ StreamWriter) (*JobResult, error) {
	if h.cfg.Enabled == nil || !h.cfg.Enabled() {
		return &JobResult{Status: JobStatusFailure, Error: ErrLaneInventoryDisabled, Output: map[string]any{"code": "lane_inventory_disabled"}}, nil
	}
	fail := func(message string) (*JobResult, error) {
		return &JobResult{Status: JobStatusFailure, Error: errors.New(message)}, nil
	}
	// Only the Redis adapters bind SourceQueue to the stream actually read.
	// Websocket's caller-selected queue label is not this proof. Direct Redis
	// also needs operator-controlled producers: stream placement alone does
	// not prove a caller's key grants (the paired API boundary is #14139).
	if !inventoryQueueID.MatchString(h.cfg.OrgID) || !inventoryQueueID.MatchString(h.cfg.NodeID) || job == nil || (job.Source != "redis" && job.Source != "redis-api") || job.SourceQueue != "jobs:v1:shell:org_"+h.cfg.OrgID+":node:"+h.cfg.NodeID {
		return fail("LANE_INVENTORY requires this node's per-node stream")
	}
	if h.cfg.Collect == nil {
		return fail("LANE_INVENTORY collector unavailable")
	}
	report, err := h.cfg.Collect(ctx)
	if err != nil {
		return fail("LANE_INVENTORY observation unavailable")
	}
	raw, err := laneinventory.Encode(report)
	if err != nil {
		return fail("LANE_INVENTORY output unavailable")
	}
	var output map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&output) != nil {
		return fail("LANE_INVENTORY output unavailable")
	}
	return &JobResult{Status: JobStatusSuccess, Output: output}, nil
}
