// Package workflow provides a workflow execution engine for Citadel nodes.
//
// It accepts WorkflowGraph JSON (matching the AceTeam platform format),
// resolves the DAG via topological sort, and executes nodes sequentially.
// Built-in node types: Input, Output, LLM, Shell, HTTP, Transform.
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// WorkflowGraph is the top-level structure submitted for execution.
type WorkflowGraph struct {
	InputNode  *Node   `json:"input_node"`
	OutputNode *Node   `json:"output_node"`
	InnerNodes []*Node `json:"inner_nodes"`
	Edges      []*Edge `json:"edges"`
}

type Node struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Params map[string]any `json:"params,omitempty"`
}

type Edge struct {
	SourceID  string `json:"source_id"`
	SourceKey string `json:"source_key"`
	TargetID  string `json:"target_id"`
	TargetKey string `json:"target_key"`
}

const (
	NodeTypeInput     = "Input"
	NodeTypeOutput    = "Output"
	NodeTypeLLM       = "LLM"
	NodeTypeShell     = "Shell"
	NodeTypeHTTP      = "HTTP"
	NodeTypeTransform = "Transform"
)

type ExecutionStatus string

const (
	StatusPending   ExecutionStatus = "pending"
	StatusRunning   ExecutionStatus = "running"
	StatusCompleted ExecutionStatus = "completed"
	StatusFailed    ExecutionStatus = "failed"
	StatusCancelled ExecutionStatus = "cancelled"
)

type RunRequest struct {
	Graph   *WorkflowGraph `json:"graph"`
	Input   map[string]any `json:"input"`
	Timeout int            `json:"timeout,omitempty"`
}

type RunResponse struct {
	ID     string          `json:"id"`
	Status ExecutionStatus `json:"status"`
}

type Execution struct {
	mu        sync.RWMutex
	ID        string            `json:"id"`
	Status    ExecutionStatus   `json:"status"`
	Graph     *WorkflowGraph    `json:"graph"`
	Input     map[string]any    `json:"input"`
	Output    map[string]any    `json:"output,omitempty"`
	Error     string            `json:"error,omitempty"`
	StartedAt time.Time         `json:"started_at"`
	EndedAt   *time.Time        `json:"ended_at,omitempty"`
	NodeLogs  []*NodeLog        `json:"node_logs,omitempty"`
	Metrics   *ExecutionMetrics `json:"metrics,omitempty"`
}

type NodeLog struct {
	NodeID    string         `json:"node_id"`
	NodeType  string         `json:"node_type"`
	Status    string         `json:"status"`
	Output    map[string]any `json:"output,omitempty"`
	Error     string         `json:"error,omitempty"`
	StartedAt time.Time      `json:"started_at"`
	Duration  time.Duration  `json:"duration_ms"`
}

type ExecutionMetrics struct {
	TotalDuration time.Duration `json:"total_duration_ms"`
	NodesExecuted int           `json:"nodes_executed"`
}

type ShellConfig struct {
	AllowList    []string
	DenyList     []string
	WorkspaceDir string
}

type ExecutorConfig struct {
	DefaultTimeout    time.Duration
	MaxConcurrentRuns int
	Shell             ShellConfig
}

func (g *WorkflowGraph) Validate() error {
	if g.InputNode == nil {
		return errors.New("workflow graph missing input_node")
	}
	if g.OutputNode == nil {
		return errors.New("workflow graph missing output_node")
	}
	if g.InputNode.Type != NodeTypeInput {
		return fmt.Errorf("input_node must have type %q, got %q", NodeTypeInput, g.InputNode.Type)
	}
	if g.OutputNode.Type != NodeTypeOutput {
		return fmt.Errorf("output_node must have type %q, got %q", NodeTypeOutput, g.OutputNode.Type)
	}
	ids := make(map[string]bool)
	ids[g.InputNode.ID] = true
	ids[g.OutputNode.ID] = true
	if g.InputNode.ID == g.OutputNode.ID {
		return fmt.Errorf("input_node and output_node must have different IDs")
	}
	for _, n := range g.InnerNodes {
		if n.ID == "" {
			return errors.New("inner node has empty ID")
		}
		if ids[n.ID] {
			return fmt.Errorf("duplicate node ID: %q", n.ID)
		}
		ids[n.ID] = true
		switch n.Type {
		case NodeTypeLLM, NodeTypeShell, NodeTypeHTTP, NodeTypeTransform:
		default:
			return fmt.Errorf("unknown node type %q on node %q", n.Type, n.ID)
		}
	}
	for i, e := range g.Edges {
		if e.SourceID == "" || e.TargetID == "" {
			return fmt.Errorf("edge %d has empty source_id or target_id", i)
		}
		if !ids[e.SourceID] {
			return fmt.Errorf("edge %d references unknown source_id %q", i, e.SourceID)
		}
		if !ids[e.TargetID] {
			return fmt.Errorf("edge %d references unknown target_id %q", i, e.TargetID)
		}
		if e.SourceKey == "" || e.TargetKey == "" {
			return fmt.Errorf("edge %d has empty source_key or target_key", i)
		}
	}
	return nil
}

func (g *WorkflowGraph) AllNodes() []*Node {
	nodes := make([]*Node, 0, 2+len(g.InnerNodes))
	nodes = append(nodes, g.InputNode)
	nodes = append(nodes, g.InnerNodes...)
	nodes = append(nodes, g.OutputNode)
	return nodes
}

func NewExecution(graph *WorkflowGraph, input map[string]any) *Execution {
	return &Execution{
		ID:        uuid.New().String(),
		Status:    StatusPending,
		Graph:     graph,
		Input:     input,
		StartedAt: time.Now(),
	}
}

func (e *Execution) SetStatus(s ExecutionStatus) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Status = s
}

func (e *Execution) Complete(output map[string]any, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	e.EndedAt = &now
	if err != nil {
		e.Status = StatusFailed
		e.Error = err.Error()
	} else {
		e.Status = StatusCompleted
		e.Output = output
	}
}

func (e *Execution) AddNodeLog(log *NodeLog) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.NodeLogs = append(e.NodeLogs, log)
}

func (e *Execution) Snapshot() Execution {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return Execution{
		ID:        e.ID,
		Status:    e.Status,
		Graph:     e.Graph,
		Input:     e.Input,
		Output:    e.Output,
		Error:     e.Error,
		StartedAt: e.StartedAt,
		EndedAt:   e.EndedAt,
		NodeLogs:  e.NodeLogs,
		Metrics:   e.Metrics,
	}
}

func (e *Execution) MarshalJSON() ([]byte, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	type nodeLogJSON struct {
		NodeID     string         `json:"node_id"`
		NodeType   string         `json:"node_type"`
		Status     string         `json:"status"`
		Output     map[string]any `json:"output,omitempty"`
		Error      string         `json:"error,omitempty"`
		StartedAt  time.Time      `json:"started_at"`
		DurationMs int64          `json:"duration_ms"`
	}
	type metricsJSON struct {
		TotalDurationMs int64 `json:"total_duration_ms"`
		NodesExecuted   int   `json:"nodes_executed"`
	}
	type alias struct {
		ID        string          `json:"id"`
		Status    ExecutionStatus `json:"status"`
		Input     map[string]any  `json:"input,omitempty"`
		Output    map[string]any  `json:"output,omitempty"`
		Error     string          `json:"error,omitempty"`
		StartedAt time.Time       `json:"started_at"`
		EndedAt   *time.Time      `json:"ended_at,omitempty"`
		NodeLogs  []nodeLogJSON   `json:"node_logs,omitempty"`
		Metrics   *metricsJSON    `json:"metrics,omitempty"`
	}
	a := alias{
		ID: e.ID, Status: e.Status, Input: e.Input, Output: e.Output,
		Error: e.Error, StartedAt: e.StartedAt, EndedAt: e.EndedAt,
	}
	for _, nl := range e.NodeLogs {
		a.NodeLogs = append(a.NodeLogs, nodeLogJSON{
			NodeID: nl.NodeID, NodeType: nl.NodeType, Status: nl.Status,
			Output: nl.Output, Error: nl.Error, StartedAt: nl.StartedAt,
			DurationMs: nl.Duration.Milliseconds(),
		})
	}
	if e.Metrics != nil {
		a.Metrics = &metricsJSON{
			TotalDurationMs: e.Metrics.TotalDuration.Milliseconds(),
			NodesExecuted:   e.Metrics.NodesExecuted,
		}
	}
	return json.Marshal(a)
}
