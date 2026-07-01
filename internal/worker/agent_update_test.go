package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/update"
)

// perNodeQueue is a representative per-node shell stream name. The ":node:"
// segment is what the privilege gate keys on.
const perNodeQueue = "jobs:v1:shell:org_test:node:1008"

// newTestHandler builds an AgentUpdateHandler with every side effect stubbed so
// no test touches the network, filesystem, or the running process. Callers
// override individual fields via the mutator.
func newTestHandler(t *testing.T, mutate func(*AgentUpdateConfig)) *AgentUpdateHandler {
	t.Helper()
	cfg := AgentUpdateConfig{
		Version: "v2.46.0",
		GetRelease: func(target string) (*update.Release, error) {
			return &update.Release{TagName: "v2.47.0"}, nil
		},
		Download:    func(*update.Release, string) error { return nil },
		Apply:       func(string) error { return nil },
		PendingPath: "/tmp/does-not-matter",
		IsService:   func() bool { return true },
		Restart:     func() error { return nil },
		RecordState: func(string, string) {},
		// Fast idle polling so the ordering test doesn't wait on the 500ms default.
		IdlePollInterval: time.Millisecond,
		IdleTimeout:      2 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewAgentUpdateHandler(cfg)
}

func agentUpdateJob(sourceQueue string, payload map[string]any) *Job {
	if payload == nil {
		payload = map[string]any{}
	}
	return &Job{ID: "job-1", Type: JobTypeAgentUpdate, SourceQueue: sourceQueue, Payload: payload}
}

// TestAgentUpdateAlreadyLatest: when GetRelease reports (nil, nil), the handler
// returns a structured success with reason "already-latest" and never touches
// Apply or Restart.
func TestAgentUpdateAlreadyLatest(t *testing.T) {
	var applied, restarted bool
	h := newTestHandler(t, func(c *AgentUpdateConfig) {
		c.GetRelease = func(string) (*update.Release, error) { return nil, nil }
		c.Apply = func(string) error { applied = true; return nil }
		c.Restart = func() error { restarted = true; return nil }
	})

	res, err := h.Execute(context.Background(), agentUpdateJob(perNodeQueue, nil), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %v, want success", res.Status)
	}
	if res.Output["reason"] != "already-latest" {
		t.Errorf("reason = %v, want already-latest", res.Output["reason"])
	}
	if res.Output["updated"] != false {
		t.Errorf("updated = %v, want false", res.Output["updated"])
	}
	if applied {
		t.Error("Apply was called on the already-latest path")
	}
	// Give any (erroneously) armed goroutine a chance to fire.
	time.Sleep(20 * time.Millisecond)
	if restarted {
		t.Error("Restart was called on the already-latest path")
	}
}

// TestAgentUpdateTargetVersionPassthrough: an explicit target_version payload is
// parsed and forwarded verbatim to GetRelease.
func TestAgentUpdateTargetVersionPassthrough(t *testing.T) {
	var gotTarget string
	h := newTestHandler(t, func(c *AgentUpdateConfig) {
		c.GetRelease = func(target string) (*update.Release, error) {
			gotTarget = target
			return &update.Release{TagName: "v2.99.0"}, nil
		}
		// Don't actually restart in this test.
		c.IsService = func() bool { return false }
	})

	_, err := h.Execute(context.Background(), agentUpdateJob(perNodeQueue, map[string]any{"target_version": "v2.99.0"}), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotTarget != "v2.99.0" {
		t.Errorf("target forwarded to GetRelease = %q, want v2.99.0", gotTarget)
	}
}

// TestAgentUpdateEmptyTargetDefaultsLatest: no target_version yields an empty
// string target ("latest") to GetRelease.
func TestAgentUpdateEmptyTargetDefaultsLatest(t *testing.T) {
	var gotTarget = "sentinel"
	h := newTestHandler(t, func(c *AgentUpdateConfig) {
		c.GetRelease = func(target string) (*update.Release, error) {
			gotTarget = target
			return nil, nil
		}
	})
	_, err := h.Execute(context.Background(), agentUpdateJob(perNodeQueue, nil), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotTarget != "" {
		t.Errorf("target = %q, want empty (latest)", gotTarget)
	}
}

// TestAgentUpdatePerNodeGate: a job that did NOT arrive on the per-node stream
// is refused with a structured failure, and no install/restart happens.
func TestAgentUpdatePerNodeGate(t *testing.T) {
	cases := []struct {
		name        string
		sourceQueue string
		wantRefused bool
	}{
		{"shared org pool", "jobs:v1:shell:org_test", true},
		{"generic queue", "jobs:v1:cpu-general", true},
		{"empty source", "", true},
		{"per-node stream", perNodeQueue, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var applied bool
			h := newTestHandler(t, func(c *AgentUpdateConfig) {
				c.Apply = func(string) error { applied = true; return nil }
				c.IsService = func() bool { return false } // avoid restart in the accepted case
			})
			res, err := h.Execute(context.Background(), agentUpdateJob(tc.sourceQueue, nil), &NoOpStreamWriter{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantRefused {
				if res.Status != JobStatusFailure {
					t.Fatalf("status = %v, want failure (refused)", res.Status)
				}
				if applied {
					t.Error("Apply ran despite the per-node gate refusing the job")
				}
			} else {
				if res.Status != JobStatusSuccess {
					t.Fatalf("status = %v, want success (accepted)", res.Status)
				}
				if !applied {
					t.Error("Apply did not run for an accepted per-node job")
				}
			}
		})
	}
}

// TestAgentUpdateServiceContextGating: in a foreground (non-service) run the
// handler installs but does NOT restart, returning "restart required".
func TestAgentUpdateServiceContextGating(t *testing.T) {
	var restarted, drained bool
	h := newTestHandler(t, func(c *AgentUpdateConfig) {
		c.IsService = func() bool { return false }
		c.Restart = func() error { restarted = true; return nil }
		c.Drain = func() { drained = true }
	})

	res, err := h.Execute(context.Background(), agentUpdateJob(perNodeQueue, nil), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %v, want success", res.Status)
	}
	if res.Output["updated"] != true {
		t.Errorf("updated = %v, want true", res.Output["updated"])
	}
	if res.Output["restarting"] != false {
		t.Errorf("restarting = %v, want false in foreground", res.Output["restarting"])
	}
	time.Sleep(20 * time.Millisecond)
	if restarted {
		t.Error("Restart was called in a foreground (non-service) run")
	}
	if drained {
		t.Error("Drain was called in a foreground run (would wedge the worker)")
	}
}

// TestAgentUpdateReportBeforeRestartOrdering is the crux test. It proves the
// handler returns its SUCCESS result BEFORE Restart is invoked, and that Restart
// only fires once the node is idle (ActiveJobs()==0) — the post-ack signal.
//
// We simulate the runner's lifecycle: this job is "active" (ActiveJobs()==1)
// while Execute runs and its result is being published/acked. The test asserts:
//  1. Execute returns a success result while a restart has NOT yet happened.
//  2. Drain() was called (closes the new-job pickup race).
//  3. Restart fires only after we drop ActiveJobs to 0 (mirroring the runner's
//     post-ack activeJobs decrement), never before.
func TestAgentUpdateReportBeforeRestartOrdering(t *testing.T) {
	var active int64 = 1 // this job is in flight until we say otherwise

	var mu sync.Mutex
	var events []string
	record := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}

	restartCh := make(chan struct{})
	h := newTestHandler(t, func(c *AgentUpdateConfig) {
		c.ActiveJobs = func() int { return int(atomic.LoadInt64(&active)) }
		c.Drain = func() { record("drain") }
		c.Restart = func() error {
			record("restart")
			close(restartCh)
			return nil
		}
	})

	res, err := h.Execute(context.Background(), agentUpdateJob(perNodeQueue, nil), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusSuccess || res.Output["updated"] != true {
		t.Fatalf("unexpected result: %+v", res.Output)
	}
	record("result-returned")

	// The restart must NOT have fired yet: the node is still "active" (this job's
	// own ack has not been simulated). Assert restart is still pending.
	select {
	case <-restartCh:
		t.Fatal("Restart fired before the job result was acked (ActiveJobs still 1)")
	case <-time.After(30 * time.Millisecond):
		// good: still waiting for idle
	}

	// Drain must already have been called (before Execute returned), so the
	// runner stops fetching new jobs while the restart is pending.
	mu.Lock()
	if len(events) == 0 || events[0] != "drain" {
		mu.Unlock()
		t.Fatalf("Drain was not called before Execute returned; events=%v", events)
	}
	mu.Unlock()

	// Now simulate the runner publishing + acking this job: activeJobs -> 0.
	atomic.StoreInt64(&active, 0)

	select {
	case <-restartCh:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("Restart never fired after the node went idle")
	}

	// Final ordering assertion: result-returned precedes restart.
	mu.Lock()
	defer mu.Unlock()
	var resultIdx, restartIdx = -1, -1
	for i, e := range events {
		switch e {
		case "result-returned":
			resultIdx = i
		case "restart":
			restartIdx = i
		}
	}
	if resultIdx == -1 || restartIdx == -1 {
		t.Fatalf("missing events: %v", events)
	}
	if restartIdx < resultIdx {
		t.Fatalf("restart happened before result was returned: %v", events)
	}
}

// TestAgentUpdateCheckFailure: an update-check error is reported as a structured
// failure rather than hanging or panicking.
func TestAgentUpdateCheckFailure(t *testing.T) {
	h := newTestHandler(t, func(c *AgentUpdateConfig) {
		c.GetRelease = func(string) (*update.Release, error) { return nil, errors.New("github unreachable") }
	})
	res, err := h.Execute(context.Background(), agentUpdateJob(perNodeQueue, nil), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("Execute should return the failure in the result, not as err: %v", err)
	}
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure", res.Status)
	}
	if res.Output["error"] == nil {
		t.Error("failure result missing structured error field")
	}
}

// TestAgentUpdateInstallFailure: an Apply error keeps the current version and is
// reported structurally; no restart is armed.
func TestAgentUpdateInstallFailure(t *testing.T) {
	var restarted bool
	h := newTestHandler(t, func(c *AgentUpdateConfig) {
		c.Apply = func(string) error { return errors.New("checksum mismatch on swap") }
		c.Restart = func() error { restarted = true; return nil }
	})
	res, err := h.Execute(context.Background(), agentUpdateJob(perNodeQueue, nil), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure", res.Status)
	}
	time.Sleep(20 * time.Millisecond)
	if restarted {
		t.Error("Restart was armed despite the install failing")
	}
}

// TestAgentUpdateRestartAfterAckEndToEnd drives the handler through a *real*
// Runner (not injected ActiveJobs/Drain) to prove the actual guarantee this PR
// exists for: the job's result is published + acked BEFORE the restart fires.
// The handler borrows runner.ActiveJobs/runner.Drain exactly as cmd/work.go
// wires it, and the injected Restart records how many jobs the source had acked
// at the moment it was called — which must be >= 1 (this job).
func TestAgentUpdateRestartAfterAckEndToEnd(t *testing.T) {
	source := NewMockJobSource("test", []*Job{
		agentUpdateJob(perNodeQueue, nil),
	})

	runner := NewRunner(source, nil, RunnerConfig{
		WorkerID:       "w",
		MaxConcurrency: 1,
		ActivityFn:     func(string, string) {},
	})

	restarted := make(chan int, 1)
	runner.RegisterHandler(NewAgentUpdateHandler(AgentUpdateConfig{
		Version:          "v2.46.0",
		GetRelease:       func(string) (*update.Release, error) { return &update.Release{TagName: "v2.47.0"}, nil },
		Download:         func(*update.Release, string) error { return nil },
		Apply:            func(string) error { return nil },
		RecordState:      func(string, string) {},
		IsService:        func() bool { return true },
		Drain:            func() { runner.Drain() },
		ActiveJobs:       runner.ActiveJobs,
		IdlePollInterval: time.Millisecond,
		IdleTimeout:      2 * time.Second,
		Restart: func() error {
			// At restart time, this job must already be acked.
			restarted <- len(source.AckedJobs())
			return nil
		},
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = runner.Run(ctx); close(done) }()

	select {
	case ackedAtRestart := <-restarted:
		if ackedAtRestart < 1 {
			t.Fatalf("restart fired with %d acked jobs; the result was not acked first", ackedAtRestart)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restart never fired end-to-end")
	}

	// The job must be in the acked set (success), not nacked (failure).
	if got := len(source.AckedJobs()); got != 1 {
		t.Errorf("acked jobs = %d, want 1", got)
	}
	if got := len(source.NackedJobs()); got != 0 {
		t.Errorf("nacked jobs = %d, want 0 (job should have succeeded)", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not shut down")
	}
}

// TestAgentUpdateCanHandle guards the dispatch key.
func TestAgentUpdateCanHandle(t *testing.T) {
	h := NewAgentUpdateHandler(AgentUpdateConfig{Version: "v1"})
	if !h.CanHandle(JobTypeAgentUpdate) {
		t.Error("handler should claim AGENT_UPDATE")
	}
	if h.CanHandle("SHELL_COMMAND") {
		t.Error("handler must not claim other job types")
	}
}
