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
}

type LaneInventoryHandler struct{ cfg LaneInventoryConfig }

func NewLaneInventoryHandler(cfg LaneInventoryConfig) *LaneInventoryHandler {
	return &LaneInventoryHandler{cfg: cfg}
}
func (*LaneInventoryHandler) CanHandle(kind string) bool { return kind == JobTypeLaneInventory }

var inventoryQueueID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (h *LaneInventoryHandler) Execute(ctx context.Context, job *Job, _ StreamWriter) (*JobResult, error) {
	fail := func(message string) (*JobResult, error) {
		return &JobResult{Status: JobStatusFailure, Error: errors.New(message)}, nil
	}
	// SourceQueue is source-owned, not a caller payload assertion. Match the
	// complete own-node name, not just a :node: substring or foreign node.
	if !inventoryQueueID.MatchString(h.cfg.OrgID) || !inventoryQueueID.MatchString(h.cfg.NodeID) || job == nil || job.SourceQueue != "jobs:v1:shell:org_"+h.cfg.OrgID+":node:"+h.cfg.NodeID {
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
