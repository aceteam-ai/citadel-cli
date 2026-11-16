package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/laneinventory"
)

func TestLaneInventoryKnownJob(t *testing.T) {
	for _, kind := range allKnownJobTypes {
		if kind == "LANE_INVENTORY" {
			return
		}
	}
	t.Fatal("the per-node inventory job is not known by this worker")
}

func TestLaneInventoryExactOwnStream(t *testing.T) {
	for _, queue := range []string{"", "jobs:v1:cpu", "jobs:v1:shell:org_org", "jobs:v1:shell:org_other:node:42", "jobs:v1:shell:org_org:node:43", "fake:node:42", "jobs:v1:shell:org_org:node:42"} {
		t.Run(queue, func(t *testing.T) {
			called := 0
			h := NewLaneInventoryHandler(LaneInventoryConfig{OrgID: "org", NodeID: "42", Collect: func(context.Context) (laneinventory.Report, error) {
				called++
				return laneinventory.Report{Panes: []laneinventory.Pane{}}, nil
			}})
			job := &Job{Type: JobTypeLaneInventory, SourceQueue: queue, Payload: map[string]any{"source_queue": "jobs:v1:shell:org_org:node:42", "cwd": "/forbidden", "command": "env"}}
			r, err := h.Execute(context.Background(), job, nil)
			if err != nil {
				t.Fatal(err)
			}
			allowed := queue == "jobs:v1:shell:org_org:node:42"
			if (r.Status == JobStatusSuccess) != allowed || (called == 1) != allowed {
				t.Fatalf("status=%s called=%d", r.Status, called)
			}
			if !h.CanHandle(JobTypeLaneInventory) || h.CanHandle(JobTypeShellCommand) {
				t.Fatal("handler routing")
			}
		})
	}
}

func TestLaneInventoryRefusalAndOutputBound(t *testing.T) {
	job := &Job{Type: JobTypeLaneInventory, SourceQueue: "jobs:v1:shell:org_org:node:42"}
	for _, cfg := range []LaneInventoryConfig{
		{OrgID: "org", NodeID: "42"},
		{OrgID: "", NodeID: "42"},
		{OrgID: "org:node:foreign", NodeID: "42"},
		{OrgID: "org", NodeID: "42", Collect: func(context.Context) (laneinventory.Report, error) {
			return laneinventory.Report{}, errors.New("planted-sensitive-error")
		}},
	} {
		r, _ := NewLaneInventoryHandler(cfg).Execute(context.Background(), job, nil)
		if r.Status != JobStatusFailure || strings.Contains(r.Error.Error(), "planted") {
			t.Fatal("failure not sanitized")
		}
	}
	h := NewLaneInventoryHandler(LaneInventoryConfig{OrgID: "org", NodeID: "42", Collect: func(context.Context) (laneinventory.Report, error) {
		r := laneinventory.Report{}
		for i := 0; i < 128; i++ {
			r.Panes = append(r.Panes, laneinventory.Pane{Session: strings.Repeat("x", 4096)})
		}
		return r, nil
	}})
	r, err := h.Execute(context.Background(), job, nil)
	if err != nil || r.Status != JobStatusSuccess {
		t.Fatalf("%+v %v", r, err)
	}
	raw, _ := json.Marshal(r.Output)
	if len(raw) > laneinventory.MaxBytes || r.Output["truncated"] != true {
		t.Fatalf("unbounded output: %d", len(raw))
	}
}
