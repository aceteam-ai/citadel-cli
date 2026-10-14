package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

func TestClockActualHandlerMetricsOutputMilliseconds(t *testing.T) {
	// Only pure input/output nodes are installed: no shell, HTTP, model, host
	// discovery or production constructor is reachable from this fixture.
	executor := &Executor{runs: make(map[string]*Execution), sem: make(chan struct{}, 1), config: ExecutorConfig{DefaultTimeout: time.Second}, nodeRegistry: map[string]NodeExecutor{NodeTypeInput: &InputNodeExecutor{}, NodeTypeOutput: &OutputNodeExecutor{}}}
	job := &worker.Job{ID: "synthetic-clock-workflow", Type: JobType, SourceQueue: testPerNodeQueue, Payload: map[string]any{
		"graph": map[string]any{
			"input_node":  map[string]any{"id": "in", "type": "Input"},
			"output_node": map[string]any{"id": "out", "type": "Output"},
			"inner_nodes": []any{},
			"edges":       []any{map[string]any{"source_id": "in", "source_key": "value", "target_id": "out", "target_key": "value"}},
		}, "input": map[string]any{"value": "synthetic"},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := NewHandler(executor).Execute(ctx, job, &worker.NoOpStreamWriter{})
	if err != nil || result == nil || result.Status != worker.JobStatusSuccess {
		t.Fatalf("pure fixture failed: %v", err)
	}
	metrics, ok := result.Output["metrics"].(*ExecutionMetrics)
	if !ok || metrics.TotalDuration <= 0 {
		t.Fatal("handler metrics fixture absent")
	}
	b, err := json.Marshal(result.Output)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Metrics struct {
			TotalDuration int64 `json:"total_duration_ms"`
		} `json:"metrics"`
	}
	if err = json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Metrics.TotalDuration != metrics.TotalDuration.Milliseconds() {
		t.Fatalf("handler bypass still emits nanoseconds: %s", b)
	}
}
