package workflow

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

type cancelBlockedNode struct {
	started  chan context.Context
	observed chan struct{}
	release  chan struct{}
	once     sync.Once
}

func newCancelBlockedNode() *cancelBlockedNode {
	return &cancelBlockedNode{
		started: make(chan context.Context, 1), observed: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (n *cancelBlockedNode) finish() { n.once.Do(func() { close(n.release) }) }

func (n *cancelBlockedNode) Execute(ctx context.Context, _ *Node, _ map[string]any) (map[string]any, error) {
	n.started <- ctx
	<-ctx.Done()
	close(n.observed)
	<-n.release // Deliberately hold actual node exit after cancellation.
	return map[string]any{"late_output": true}, nil
}

func cancellationGraph() *WorkflowGraph {
	return &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		InnerNodes: []*Node{{ID: "blocked", Type: NodeTypeLLM}},
		Edges: []*Edge{
			{SourceID: "in", SourceKey: "value", TargetID: "blocked", TargetKey: "value"},
			{SourceID: "blocked", SourceKey: "late_output", TargetID: "out", TargetKey: "value"},
		},
	}
}

func awaitWorkflowSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("owned workflow signal did not arrive")
	}
}

func awaitWorkflowStart(t *testing.T, node *cancelBlockedNode) context.Context {
	t.Helper()
	select {
	case ctx := <-node.started:
		return ctx
	case <-time.After(2 * time.Second):
		t.Fatal("owned workflow node did not start")
		return nil
	}
}

func TestExecutorCancelDeliversContextAndRetainsSlotUntilActualExit(t *testing.T) {
	e := NewExecutor(ExecutorConfig{DefaultTimeout: time.Minute})
	node := newCancelBlockedNode()
	e.nodeRegistry[NodeTypeLLM] = node
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); node.finish() })
	run, err := e.Submit(ctx, &RunRequest{Graph: cancellationGraph()})
	if err != nil {
		t.Fatal(err)
	}
	nodeCtx := awaitWorkflowStart(t, node)
	if !e.Cancel(run.ID) || e.Cancel(run.ID) {
		t.Fatal("cancellation must be accepted exactly once")
	}
	awaitWorkflowSignal(t, node.observed)
	if nodeCtx.Err() != context.Canceled || e.ActiveCount() != 1 {
		t.Fatal("context cancellation was lost or the live node's slot was released")
	}
	select {
	case <-run.Done():
		t.Fatal("cancelled status was mistaken for actual node exit")
	default:
	}
	node.finish()
	awaitWorkflowSignal(t, run.Done())
	snap := run.Snapshot()
	if snap.Status != StatusCancelled || snap.EndedAt == nil || snap.Output != nil || e.ActiveCount() != 0 {
		t.Fatal("late output/graph completion overwrote cancellation or retained its slot")
	}
	run.SetStatus(StatusRunning)
	run.Complete(map[string]any{"late": true}, nil)
	if run.Snapshot().Status != StatusCancelled {
		t.Fatal("a terminal cancellation was resurrected")
	}
	run.mu.RLock()
	retained := run.cancel != nil
	run.mu.RUnlock()
	if retained {
		t.Fatal("finished run retained its cancellation function")
	}
}

func TestExecutorCancelBeforeGraphStartupCannotRunANode(t *testing.T) {
	e := NewExecutor(ExecutorConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := NewExecution(cancellationGraph(), nil)
	run.cancel = cancel
	e.runs[run.ID] = run
	if !e.Cancel(run.ID) {
		t.Fatal("pending cancellation refused")
	}
	e.executeGraph(ctx, run)
	snap := run.Snapshot()
	if ctx.Err() != context.Canceled || snap.Status != StatusCancelled || len(snap.NodeLogs) != 0 {
		t.Fatal("pre-start cancellation was lost or ran a node")
	}
}

func TestExecutorRejectedRunReleasesItsCancellationAndExitSignal(t *testing.T) {
	e := NewExecutor(ExecutorConfig{DefaultTimeout: time.Minute, MaxConcurrentRuns: 1})
	node := newCancelBlockedNode()
	e.nodeRegistry[NodeTypeLLM] = node
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); node.finish() })
	first, err := e.Submit(ctx, &RunRequest{Graph: cancellationGraph()})
	if err != nil {
		t.Fatal(err)
	}
	awaitWorkflowStart(t, node)
	rejected, err := e.Submit(ctx, &RunRequest{Graph: cancellationGraph()})
	if err != nil {
		t.Fatal(err)
	}
	awaitWorkflowSignal(t, rejected.Done())
	if rejected.Snapshot().Status != StatusFailed || e.Cancel(rejected.ID) || e.ActiveCount() != 1 {
		t.Fatal("capacity rejection leaked a slot or changed terminal state")
	}
	rejected.mu.RLock()
	retained := rejected.cancel != nil
	rejected.mu.RUnlock()
	if retained {
		t.Fatal("rejected run retained its timeout/cancellation function")
	}
	e.Cancel(first.ID)
	node.finish()
	awaitWorkflowSignal(t, first.Done())
}

func TestWorkflowHandlerCancellationWaitsForActualNodeExit(t *testing.T) {
	e := NewExecutor(ExecutorConfig{DefaultTimeout: time.Minute})
	node := newCancelBlockedNode()
	e.nodeRegistry[NodeTypeLLM] = node
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); node.finish() })
	graph, err := json.Marshal(cancellationGraph())
	if err != nil {
		t.Fatal(err)
	}
	job := &worker.Job{ID: "owned-cancellation-fixture", Type: JobType, SourceQueue: testPerNodeQueue,
		Payload: map[string]any{"graph": string(graph)}}
	result := make(chan error, 1)
	go func() {
		_, err := NewHandler(e).Execute(ctx, job, &worker.NoOpStreamWriter{})
		result <- err
	}()
	awaitWorkflowStart(t, node)
	cancel()
	awaitWorkflowSignal(t, node.observed)
	select {
	case <-result:
		t.Fatal("worker handler returned while the cancelled node still held exit")
	case <-time.After(100 * time.Millisecond):
	}
	node.finish()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled workflow reported worker success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker handler did not return after actual node exit")
	}
}
