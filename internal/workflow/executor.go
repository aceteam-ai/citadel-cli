package workflow

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	defaultTimeout       = 5 * time.Minute
	defaultMaxConcurrent = 16
)

type Executor struct {
	mu           sync.RWMutex
	runs         map[string]*Execution
	config       ExecutorConfig
	sem          chan struct{}
	nodeRegistry map[string]NodeExecutor
}

func NewExecutor(cfg ExecutorConfig) *Executor {
	if cfg.DefaultTimeout == 0 {
		cfg.DefaultTimeout = defaultTimeout
	}
	if cfg.MaxConcurrentRuns == 0 {
		cfg.MaxConcurrentRuns = defaultMaxConcurrent
	}
	return &Executor{
		runs:   make(map[string]*Execution),
		config: cfg,
		sem:    make(chan struct{}, cfg.MaxConcurrentRuns),
		nodeRegistry: map[string]NodeExecutor{
			NodeTypeInput:     &InputNodeExecutor{},
			NodeTypeOutput:    &OutputNodeExecutor{},
			NodeTypeLLM:       &LLMNodeExecutor{},
			NodeTypeShell:     NewShellNodeExecutor(cfg.Shell),
			NodeTypeHTTP:      &HTTPNodeExecutor{},
			NodeTypeTransform: &TransformNodeExecutor{},
		},
	}
}

func (e *Executor) Submit(ctx context.Context, req *RunRequest) (*Execution, error) {
	if req.Graph == nil {
		return nil, errors.New("graph is required")
	}
	if err := req.Graph.Validate(); err != nil {
		return nil, fmt.Errorf("invalid graph: %w", err)
	}
	timeout := e.config.DefaultTimeout
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}
	exec := NewExecution(req.Graph, req.Input)
	e.mu.Lock()
	e.runs[exec.ID] = exec
	e.mu.Unlock()
	select {
	case e.sem <- struct{}{}:
	default:
		exec.Complete(nil, errors.New("max concurrent workflow runs exceeded"))
		return exec, nil
	}
	go func() {
		defer func() { <-e.sem }()
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		e.executeGraph(runCtx, exec)
	}()
	return exec, nil
}

func (e *Executor) Get(id string) *Execution {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.runs[id]
}

func (e *Executor) List() []*Execution {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make([]*Execution, 0, len(e.runs))
	for _, exec := range e.runs {
		result = append(result, exec)
	}
	return result
}

func (e *Executor) Cancel(id string) bool {
	e.mu.RLock()
	exec := e.runs[id]
	e.mu.RUnlock()
	if exec == nil {
		return false
	}
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if exec.Status == StatusRunning || exec.Status == StatusPending {
		now := time.Now()
		exec.Status = StatusCancelled
		exec.EndedAt = &now
		return true
	}
	return false
}

func (e *Executor) ActiveCount() int {
	return len(e.sem)
}

func (e *Executor) DrainAndWait(ctx context.Context) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if e.ActiveCount() == 0 {
				return
			}
		}
	}
}

func (e *Executor) executeGraph(ctx context.Context, exec *Execution) {
	exec.SetStatus(StatusRunning)
	startTime := time.Now()
	graph := exec.Graph
	order, err := topologicalSort(graph)
	if err != nil {
		exec.Complete(nil, fmt.Errorf("topological sort: %w", err))
		return
	}
	nodeOutputs := make(map[string]map[string]any)
	nodesExecuted := 0
	for _, nodeID := range order {
		if ctx.Err() != nil {
			exec.Complete(nil, fmt.Errorf("workflow cancelled or timed out: %w", ctx.Err()))
			return
		}
		snap := exec.Snapshot()
		if snap.Status == StatusCancelled {
			return
		}
		node := findNode(graph, nodeID)
		if node == nil {
			exec.Complete(nil, fmt.Errorf("internal error: node %q not found in graph", nodeID))
			return
		}
		nodeInput := buildNodeInput(graph, nodeID, nodeOutputs, exec.Input)
		executor, ok := e.nodeRegistry[node.Type]
		if !ok {
			exec.Complete(nil, fmt.Errorf("no executor for node type %q", node.Type))
			return
		}
		nodeStart := time.Now()
		output, nodeErr := executor.Execute(ctx, node, nodeInput)
		nodeDuration := time.Since(nodeStart)
		nl := &NodeLog{
			NodeID: node.ID, NodeType: node.Type,
			StartedAt: nodeStart, Duration: nodeDuration,
		}
		if nodeErr != nil {
			nl.Status = "failed"
			nl.Error = nodeErr.Error()
			exec.AddNodeLog(nl)
			exec.Complete(nil, fmt.Errorf("node %q (%s) failed: %w", node.ID, node.Type, nodeErr))
			return
		}
		nl.Status = "success"
		nl.Output = output
		exec.AddNodeLog(nl)
		nodeOutputs[node.ID] = output
		nodesExecuted++
		log.Printf("[workflow] node %q (%s) completed in %s", node.ID, node.Type, nodeDuration.Round(time.Millisecond))
	}
	finalOutput := nodeOutputs[graph.OutputNode.ID]
	totalDuration := time.Since(startTime)
	exec.mu.Lock()
	exec.Metrics = &ExecutionMetrics{
		TotalDuration: totalDuration,
		NodesExecuted: nodesExecuted,
	}
	exec.mu.Unlock()
	exec.Complete(finalOutput, nil)
}

func topologicalSort(g *WorkflowGraph) ([]string, error) {
	allNodes := g.AllNodes()
	inDegree := make(map[string]int)
	adjacency := make(map[string][]string)
	for _, n := range allNodes {
		inDegree[n.ID] = 0
	}
	for _, edge := range g.Edges {
		adjacency[edge.SourceID] = append(adjacency[edge.SourceID], edge.TargetID)
		inDegree[edge.TargetID]++
	}
	var queue []string
	for _, n := range allNodes {
		if inDegree[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}
	var order []string
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		order = append(order, current)
		for _, target := range adjacency[current] {
			inDegree[target]--
			if inDegree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	if len(order) != len(allNodes) {
		return nil, errors.New("cycle detected in workflow graph")
	}
	return order, nil
}

func findNode(g *WorkflowGraph, id string) *Node {
	if g.InputNode.ID == id {
		return g.InputNode
	}
	if g.OutputNode.ID == id {
		return g.OutputNode
	}
	for _, n := range g.InnerNodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

func buildNodeInput(g *WorkflowGraph, nodeID string, nodeOutputs map[string]map[string]any, workflowInput map[string]any) map[string]any {
	if nodeID == g.InputNode.ID {
		return workflowInput
	}
	input := make(map[string]any)
	for _, edge := range g.Edges {
		if edge.TargetID != nodeID {
			continue
		}
		sourceOutput, ok := nodeOutputs[edge.SourceID]
		if !ok {
			continue
		}
		val, ok := sourceOutput[edge.SourceKey]
		if !ok {
			continue
		}
		input[edge.TargetKey] = val
	}
	return input
}
