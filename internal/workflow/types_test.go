package workflow

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestWorkflowGraph_Validate_Valid(t *testing.T) {
	g := &WorkflowGraph{
		InputNode:  &Node{ID: "in", Type: NodeTypeInput},
		OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		InnerNodes: []*Node{{ID: "llm1", Type: NodeTypeLLM}, {ID: "shell1", Type: NodeTypeShell}},
		Edges: []*Edge{
			{SourceID: "in", SourceKey: "prompt", TargetID: "llm1", TargetKey: "prompt"},
			{SourceID: "llm1", SourceKey: "response", TargetID: "shell1", TargetKey: "command"},
			{SourceID: "shell1", SourceKey: "stdout", TargetID: "out", TargetKey: "result"},
		},
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("expected valid graph, got error: %v", err)
	}
}

func TestWorkflowGraph_Validate_MissingInputNode(t *testing.T) {
	g := &WorkflowGraph{OutputNode: &Node{ID: "out", Type: NodeTypeOutput}}
	if err := g.Validate(); err == nil {
		t.Fatal("expected error for missing input_node")
	}
}

func TestWorkflowGraph_Validate_MissingOutputNode(t *testing.T) {
	g := &WorkflowGraph{InputNode: &Node{ID: "in", Type: NodeTypeInput}}
	if err := g.Validate(); err == nil {
		t.Fatal("expected error for missing output_node")
	}
}

func TestWorkflowGraph_Validate_WrongInputType(t *testing.T) {
	g := &WorkflowGraph{InputNode: &Node{ID: "in", Type: NodeTypeLLM}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput}}
	if err := g.Validate(); err == nil {
		t.Fatal("expected error for wrong input_node type")
	}
}

func TestWorkflowGraph_Validate_DuplicateNodeIDs(t *testing.T) {
	g := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		InnerNodes: []*Node{{ID: "x", Type: NodeTypeLLM}, {ID: "x", Type: NodeTypeShell}},
	}
	if err := g.Validate(); err == nil {
		t.Fatal("expected error for duplicate node IDs")
	}
}

func TestWorkflowGraph_Validate_SameInputOutputID(t *testing.T) {
	g := &WorkflowGraph{InputNode: &Node{ID: "io", Type: NodeTypeInput}, OutputNode: &Node{ID: "io", Type: NodeTypeOutput}}
	if err := g.Validate(); err == nil {
		t.Fatal("expected error when input and output share ID")
	}
}

func TestWorkflowGraph_Validate_UnknownNodeType(t *testing.T) {
	g := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		InnerNodes: []*Node{{ID: "x", Type: "FooBar"}},
	}
	if err := g.Validate(); err == nil {
		t.Fatal("expected error for unknown node type")
	}
}

func TestWorkflowGraph_Validate_EmptyInnerNodeID(t *testing.T) {
	g := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		InnerNodes: []*Node{{ID: "", Type: NodeTypeLLM}},
	}
	if err := g.Validate(); err == nil {
		t.Fatal("expected error for empty inner node ID")
	}
}

func TestWorkflowGraph_Validate_EdgeUnknownSource(t *testing.T) {
	g := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		Edges: []*Edge{{SourceID: "missing", SourceKey: "x", TargetID: "out", TargetKey: "y"}},
	}
	if err := g.Validate(); err == nil {
		t.Fatal("expected error for edge referencing unknown source")
	}
}

func TestWorkflowGraph_Validate_EdgeEmptyKeys(t *testing.T) {
	g := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		Edges: []*Edge{{SourceID: "in", SourceKey: "", TargetID: "out", TargetKey: "y"}},
	}
	if err := g.Validate(); err == nil {
		t.Fatal("expected error for edge with empty keys")
	}
}

func TestWorkflowGraph_AllNodes(t *testing.T) {
	g := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		InnerNodes: []*Node{{ID: "a", Type: NodeTypeLLM}, {ID: "b", Type: NodeTypeShell}},
	}
	nodes := g.AllNodes()
	if len(nodes) != 4 {
		t.Fatalf("expected 4 nodes, got %d", len(nodes))
	}
}

func TestNewExecution(t *testing.T) {
	g := &WorkflowGraph{InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput}}
	exec := NewExecution(g, map[string]any{"key": "value"})
	if exec.ID == "" {
		t.Fatal("execution ID should be non-empty")
	}
	if exec.Status != StatusPending {
		t.Fatalf("expected pending status, got %s", exec.Status)
	}
}

func TestExecution_Complete_Success(t *testing.T) {
	exec := &Execution{ID: "test", Status: StatusRunning}
	exec.Complete(map[string]any{"result": "ok"}, nil)
	if exec.Status != StatusCompleted {
		t.Fatalf("expected completed, got %s", exec.Status)
	}
}

func TestExecution_Complete_Failure(t *testing.T) {
	exec := &Execution{ID: "test", Status: StatusRunning}
	exec.Complete(nil, fmt.Errorf("something broke"))
	if exec.Status != StatusFailed {
		t.Fatalf("expected failed, got %s", exec.Status)
	}
}

func TestExecution_MarshalJSON(t *testing.T) {
	exec := &Execution{ID: "test-123", Status: StatusCompleted, Output: map[string]any{"result": "ok"}}
	data, err := json.Marshal(exec)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var parsed map[string]any
	json.Unmarshal(data, &parsed)
	if parsed["id"] != "test-123" {
		t.Fatalf("unexpected id: %v", parsed["id"])
	}
}

func TestRunRequest_ParseFromJSON(t *testing.T) {
	raw := `{"graph":{"input_node":{"id":"in","type":"Input"},"output_node":{"id":"out","type":"Output"},"inner_nodes":[],"edges":[{"source_id":"in","source_key":"text","target_id":"out","target_key":"result"}]},"input":{"text":"hello"},"timeout":60}`
	var req RunRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if req.Graph == nil || req.Graph.InputNode.ID != "in" || req.Timeout != 60 {
		t.Fatal("unexpected parse result")
	}
}
