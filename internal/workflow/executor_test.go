package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type mockNodeExecutor struct {
	output map[string]any
	err    error
	called int
}

func (m *mockNodeExecutor) Execute(_ context.Context, _ *Node, _ map[string]any) (map[string]any, error) {
	m.called++
	return m.output, m.err
}

func TestTopologicalSort_Linear(t *testing.T) {
	g := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		InnerNodes: []*Node{{ID: "a", Type: NodeTypeLLM}, {ID: "b", Type: NodeTypeShell}},
		Edges: []*Edge{
			{SourceID: "in", SourceKey: "x", TargetID: "a", TargetKey: "y"},
			{SourceID: "a", SourceKey: "x", TargetID: "b", TargetKey: "y"},
			{SourceID: "b", SourceKey: "x", TargetID: "out", TargetKey: "y"},
		},
	}
	order, err := topologicalSort(g)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 4 {
		t.Fatalf("expected 4 nodes, got %d", len(order))
	}
	indexOf := func(id string) int {
		for i, v := range order {
			if v == id {
				return i
			}
		}
		return -1
	}
	if indexOf("in") >= indexOf("a") || indexOf("a") >= indexOf("b") || indexOf("b") >= indexOf("out") {
		t.Fatal("wrong order")
	}
}

func TestTopologicalSort_Cycle(t *testing.T) {
	g := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		InnerNodes: []*Node{{ID: "a", Type: NodeTypeLLM}, {ID: "b", Type: NodeTypeShell}},
		Edges: []*Edge{
			{SourceID: "in", SourceKey: "x", TargetID: "a", TargetKey: "y"},
			{SourceID: "a", SourceKey: "x", TargetID: "b", TargetKey: "y"},
			{SourceID: "b", SourceKey: "x", TargetID: "a", TargetKey: "y"},
		},
	}
	if _, err := topologicalSort(g); err == nil {
		t.Fatal("expected cycle detection error")
	}
}

func TestTopologicalSort_SingleEdge(t *testing.T) {
	g := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		Edges: []*Edge{{SourceID: "in", SourceKey: "data", TargetID: "out", TargetKey: "result"}},
	}
	order, err := topologicalSort(g)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 2 || order[0] != "in" || order[1] != "out" {
		t.Fatalf("unexpected order: %v", order)
	}
}

func TestBuildNodeInput(t *testing.T) {
	g := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		InnerNodes: []*Node{{ID: "a", Type: NodeTypeLLM}},
		Edges: []*Edge{
			{SourceID: "in", SourceKey: "prompt", TargetID: "a", TargetKey: "prompt"},
			{SourceID: "a", SourceKey: "response", TargetID: "out", TargetKey: "result"},
		},
	}
	nodeOutputs := map[string]map[string]any{
		"in": {"prompt": "hello"}, "a": {"response": "world"},
	}
	result := buildNodeInput(g, "in", nodeOutputs, map[string]any{"prompt": "hello"})
	if result["prompt"] != "hello" {
		t.Fatal("input node should get workflow input")
	}
	result = buildNodeInput(g, "out", nodeOutputs, nil)
	if result["result"] != "world" {
		t.Fatal("output node should get response from node a")
	}
}

func TestExecutor_Submit_PassthroughWorkflow(t *testing.T) {
	e := NewExecutor(ExecutorConfig{DefaultTimeout: 5 * time.Second})
	graph := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		Edges: []*Edge{{SourceID: "in", SourceKey: "data", TargetID: "out", TargetKey: "result"}},
	}
	exec, err := e.Submit(context.Background(), &RunRequest{Graph: graph, Input: map[string]any{"data": "hello"}})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("workflow did not complete in time")
		default:
			snap := exec.Snapshot()
			if snap.Status == StatusCompleted {
				if snap.Output["result"] != "hello" {
					t.Fatalf("expected result='hello', got %v", snap.Output)
				}
				return
			}
			if snap.Status == StatusFailed {
				t.Fatalf("workflow failed: %s", snap.Error)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestExecutor_Submit_TransformWorkflow(t *testing.T) {
	e := NewExecutor(ExecutorConfig{DefaultTimeout: 5 * time.Second})
	graph := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		InnerNodes: []*Node{{ID: "transform", Type: NodeTypeTransform, Params: map[string]any{
			"template": map[string]any{"greeting": "Hello, {{name}}!"},
		}}},
		Edges: []*Edge{
			{SourceID: "in", SourceKey: "name", TargetID: "transform", TargetKey: "name"},
			{SourceID: "transform", SourceKey: "greeting", TargetID: "out", TargetKey: "message"},
		},
	}
	exec, err := e.Submit(context.Background(), &RunRequest{Graph: graph, Input: map[string]any{"name": "World"}})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("workflow did not complete in time")
		default:
			snap := exec.Snapshot()
			if snap.Status == StatusCompleted {
				if snap.Output["message"] != "Hello, World!" {
					t.Fatalf("expected 'Hello, World!', got %v", snap.Output)
				}
				return
			}
			if snap.Status == StatusFailed {
				t.Fatalf("workflow failed: %s", snap.Error)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestExecutor_Cancel(t *testing.T) {
	e := NewExecutor(ExecutorConfig{})
	exec := &Execution{ID: "test", Status: StatusRunning}
	e.mu.Lock()
	e.runs["test"] = exec
	e.mu.Unlock()
	if !e.Cancel("test") {
		t.Fatal("cancel should return true")
	}
	if exec.Status != StatusCancelled {
		t.Fatalf("expected cancelled, got %s", exec.Status)
	}
	if e.Cancel("test") {
		t.Fatal("second cancel should fail")
	}
	if e.Cancel("nope") {
		t.Fatal("cancel nonexistent should fail")
	}
}

func TestExecutor_Timeout(t *testing.T) {
	e := NewExecutor(ExecutorConfig{DefaultTimeout: 100 * time.Millisecond})
	e.nodeRegistry[NodeTypeLLM] = &slowNodeExecutor{delay: 5 * time.Second}
	graph := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		InnerNodes: []*Node{{ID: "slow", Type: NodeTypeLLM}},
		Edges: []*Edge{
			{SourceID: "in", SourceKey: "x", TargetID: "slow", TargetKey: "y"},
			{SourceID: "slow", SourceKey: "x", TargetID: "out", TargetKey: "y"},
		},
	}
	exec, err := e.Submit(context.Background(), &RunRequest{Graph: graph})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout test did not resolve")
		default:
			if exec.Snapshot().Status == StatusFailed {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestExecutor_NodeFailure(t *testing.T) {
	e := NewExecutor(ExecutorConfig{DefaultTimeout: 5 * time.Second})
	e.nodeRegistry[NodeTypeTransform] = &mockNodeExecutor{err: fmt.Errorf("transform failed")}
	graph := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		InnerNodes: []*Node{{ID: "fail", Type: NodeTypeTransform}},
		Edges: []*Edge{
			{SourceID: "in", SourceKey: "x", TargetID: "fail", TargetKey: "y"},
			{SourceID: "fail", SourceKey: "x", TargetID: "out", TargetKey: "y"},
		},
	}
	exec, _ := e.Submit(context.Background(), &RunRequest{Graph: graph})
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("node failure test did not resolve")
		default:
			snap := exec.Snapshot()
			if snap.Status == StatusFailed {
				if snap.Error == "" {
					t.Fatal("error should be set")
				}
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

type slowNodeExecutor struct{ delay time.Duration }

func (s *slowNodeExecutor) Execute(ctx context.Context, _ *Node, _ map[string]any) (map[string]any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.delay):
		return map[string]any{"x": "done"}, nil
	}
}
