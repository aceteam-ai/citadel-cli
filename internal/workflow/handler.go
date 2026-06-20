package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

const JobType = "WORKFLOW_RUN"

type Handler struct {
	executor *Executor
}

func NewHandler(executor *Executor) *Handler {
	return &Handler{executor: executor}
}

func (h *Handler) CanHandle(jobType string) bool {
	return jobType == JobType
}

func (h *Handler) Execute(ctx context.Context, job *worker.Job, stream worker.StreamWriter) (*worker.JobResult, error) {
	start := time.Now()
	graphRaw, ok := job.Payload["graph"]
	if !ok {
		return &worker.JobResult{
			Status: worker.JobStatusFailure,
			Error:  fmt.Errorf("job payload missing 'graph' field"),
		}, fmt.Errorf("job payload missing 'graph' field")
	}
	var graph WorkflowGraph
	switch v := graphRaw.(type) {
	case string:
		if err := json.Unmarshal([]byte(v), &graph); err != nil {
			return nil, fmt.Errorf("parse graph JSON string: %w", err)
		}
	case map[string]any:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshal graph map: %w", err)
		}
		if err := json.Unmarshal(b, &graph); err != nil {
			return nil, fmt.Errorf("parse graph from map: %w", err)
		}
	default:
		return nil, fmt.Errorf("graph must be a JSON string or object, got %T", graphRaw)
	}
	var input map[string]any
	if inputRaw, ok := job.Payload["input"]; ok {
		switch v := inputRaw.(type) {
		case string:
			if err := json.Unmarshal([]byte(v), &input); err != nil {
				return nil, fmt.Errorf("parse input JSON string: %w", err)
			}
		case map[string]any:
			input = v
		}
	}
	timeout := 0
	if t, ok := job.Payload["timeout"]; ok {
		switch v := t.(type) {
		case float64:
			timeout = int(v)
		case int:
			timeout = v
		}
	}
	req := &RunRequest{Graph: &graph, Input: input, Timeout: timeout}
	exec, err := h.executor.Submit(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("submit workflow: %w", err)
	}
	stream.WriteChunk(fmt.Sprintf("workflow %s started", exec.ID), 0)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	chunkIdx := 1
	lastNodeCount := 0
	for {
		select {
		case <-ctx.Done():
			h.executor.Cancel(exec.ID)
			return &worker.JobResult{
				Status: worker.JobStatusFailure, Error: ctx.Err(), Duration: time.Since(start),
			}, ctx.Err()
		case <-ticker.C:
			snap := exec.Snapshot()
			if len(snap.NodeLogs) > lastNodeCount {
				for i := lastNodeCount; i < len(snap.NodeLogs); i++ {
					nl := snap.NodeLogs[i]
					stream.WriteChunk(fmt.Sprintf("node %q (%s): %s", nl.NodeID, nl.NodeType, nl.Status), chunkIdx)
					chunkIdx++
				}
				lastNodeCount = len(snap.NodeLogs)
			}
			switch snap.Status {
			case StatusCompleted:
				return &worker.JobResult{
					Status: worker.JobStatusSuccess, Duration: time.Since(start),
					Output: map[string]any{"workflow_id": exec.ID, "output": snap.Output, "metrics": snap.Metrics},
				}, nil
			case StatusFailed:
				return &worker.JobResult{
					Status: worker.JobStatusFailure, Error: fmt.Errorf("workflow failed: %s", snap.Error),
					Duration: time.Since(start),
					Output:   map[string]any{"workflow_id": exec.ID, "error": snap.Error},
				}, fmt.Errorf("workflow failed: %s", snap.Error)
			case StatusCancelled:
				return &worker.JobResult{
					Status: worker.JobStatusFailure, Error: fmt.Errorf("workflow cancelled"),
					Duration: time.Since(start),
				}, fmt.Errorf("workflow cancelled")
			}
		}
	}
}

var _ worker.JobHandler = (*Handler)(nil)
