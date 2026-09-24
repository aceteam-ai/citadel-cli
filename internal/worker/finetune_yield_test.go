package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fineTuneYieldSource struct {
	*MockJobSource
	jobs    []*Job
	before  map[int]<-chan struct{}
	index   int
	demand  string
	outcome chan string
}

func (s *fineTuneYieldSource) Next(ctx context.Context) (*Job, error) {
	if s.index >= len(s.jobs) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	index := s.index
	s.index++
	if ready := s.before[index]; ready != nil {
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.jobs[index], nil
}

func (s *fineTuneYieldSource) Fail(ctx context.Context, job *Job, err error, data map[string]any) error {
	result := s.MockJobSource.Fail(ctx, job, err, data)
	if job.ID == s.demand {
		s.outcome <- "failed"
	}
	return result
}

func (s *fineTuneYieldSource) Ack(ctx context.Context, job *Job) error {
	result := s.MockJobSource.Ack(ctx, job)
	if job.ID == s.demand {
		s.outcome <- "admitted"
	}
	return result
}

func runFineTuneYieldScenario(t *testing.T, cfg FineTuneConfig, training, demand *Job, beforeDemand <-chan struct{}) (string, *MockJobHandler, *fineTuneYieldSource) {
	t.Helper()
	source := &fineTuneYieldSource{
		MockJobSource: NewMockJobSource("redis", nil),
		jobs:          []*Job{training, demand},
		before:        map[int]<-chan struct{}{1: beforeDemand},
		demand:        demand.ID,
		outcome:       make(chan string, 1),
	}
	demandHandler := NewMockJobHandler(demand.Type, false)
	runner := NewRunner(source, []JobHandler{NewFineTuneHandler(cfg), demandHandler}, RunnerConfig{
		NodeID: "1297", MaxConcurrency: 2, State: NewWorkerState(), ActivityFn: func(string, string) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	var outcome string
	select {
	case outcome = <-source.outcome:
	case <-time.After(5 * time.Second):
		t.Fatal("demand never reached a terminal admission decision")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not stop")
	}
	return outcome, demandHandler, source
}

func TestFineTuneUnconfirmedTerminationRefusesDemandAndSurvivesRestart(t *testing.T) {
	cfg, training, control, reservation := fineTuneFixture(t)
	started := make(chan struct{})
	cfg.Run = func(ctx context.Context, _ FineTuneSpec, _ func(map[string]any) error) error {
		close(started)
		<-ctx.Done()
		return &fineTuneTerminationError{cause: errors.New("rm and ps could not confirm absence")}
	}
	demand := &Job{ID: "demand-unsafe", Type: JobTypeShellCommand, SourceQueue: training.SourceQueue}
	outcome, demandHandler, source := runFineTuneYieldScenario(t, cfg, training, demand, started)
	if outcome != "failed" || len(demandHandler.executed) != 0 || reservation.release != 0 {
		t.Fatalf("outcome=%s executed=%d restore=%d", outcome, len(demandHandler.executed), reservation.release)
	}
	if len(control.critical) != 1 || control.critical[0]["status"] != "failed" {
		t.Fatalf("critical failure = %#v", control.critical)
	}
	if len(source.failed) < 1 {
		t.Fatal("demand refusal was not recorded")
	}
	hold := filepath.Join(cfg.SafetyDir, fineTuneSafetyHold)
	if _, err := os.Stat(hold); err != nil {
		t.Fatalf("durable hold missing: %v", err)
	}
	// A new worker instance must not silently forget the unresolved hold.
	restarted := NewFineTuneHandler(cfg)
	if err := restarted.YieldToDemand(); err == nil {
		t.Fatal("worker restart admitted demand with unresolved trainer")
	}
	// Simulate the documented, externally verified operator recovery. The
	// worker never removes an unsafe hold merely because another job arrived.
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if err := restarted.YieldToDemand(); err != nil {
		t.Fatalf("verified recovery remained blocked: %v", err)
	}
}

func TestFineTuneConfirmedYieldAdmitsDemand(t *testing.T) {
	cfg, training, control, reservation := fineTuneFixture(t)
	started := make(chan struct{})
	cfg.Run = func(ctx context.Context, _ FineTuneSpec, _ func(map[string]any) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	demand := &Job{ID: "demand-safe", Type: JobTypeShellCommand, SourceQueue: training.SourceQueue}
	outcome, demandHandler, _ := runFineTuneYieldScenario(t, cfg, training, demand, started)
	if outcome != "admitted" || len(demandHandler.executed) != 1 || reservation.release != 1 {
		t.Fatalf("outcome=%s executed=%d restore=%d", outcome, len(demandHandler.executed), reservation.release)
	}
	if control.updates[len(control.updates)-1]["status"] != "cancelled" {
		t.Fatalf("terminal status = %#v", control.updates)
	}
	if _, err := os.Stat(filepath.Join(cfg.SafetyDir, fineTuneSafetyHold)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed cleanup left hold: %v", err)
	}
}

func TestFineTuneTerminalPersistenceFailureRefusesDemand(t *testing.T) {
	cfg, training, control, reservation := fineTuneFixture(t)
	control.terminalErr = errors.New("canonical status unavailable")
	started := make(chan struct{})
	cfg.Run = func(ctx context.Context, _ FineTuneSpec, _ func(map[string]any) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	demand := &Job{ID: "demand-status", Type: JobTypeShellCommand, SourceQueue: training.SourceQueue}
	outcome, demandHandler, _ := runFineTuneYieldScenario(t, cfg, training, demand, started)
	if outcome != "failed" || len(demandHandler.executed) != 0 || reservation.release != 1 {
		t.Fatalf("outcome=%s executed=%d restore=%d", outcome, len(demandHandler.executed), reservation.release)
	}
	if len(control.updates) != 1 || control.updates[0]["status"] != "running" {
		t.Fatalf("false terminal status persisted: %#v", control.updates)
	}
	if _, err := os.Stat(filepath.Join(cfg.SafetyDir, fineTuneSafetyHold)); err != nil {
		t.Fatalf("status failure lost safety hold: %v", err)
	}
}

func TestFineTuneRestoreFailureRefusesDemandWithDurableHold(t *testing.T) {
	cfg, training, control, reservation := fineTuneFixture(t)
	reservation.releaseErr = errors.New("restore could not be confirmed")
	started := make(chan struct{})
	cfg.Run = func(ctx context.Context, _ FineTuneSpec, _ func(map[string]any) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	demand := &Job{ID: "demand-restore", Type: JobTypeShellCommand, SourceQueue: training.SourceQueue}
	outcome, demandHandler, _ := runFineTuneYieldScenario(t, cfg, training, demand, started)
	if outcome != "failed" || len(demandHandler.executed) != 0 || reservation.release != 1 {
		t.Fatalf("outcome=%s executed=%d restore=%d", outcome, len(demandHandler.executed), reservation.release)
	}
	if len(control.critical) != 1 || control.critical[0]["status"] != "failed" {
		t.Fatalf("restore failure not visible: %#v", control.critical)
	}
	if _, err := os.Stat(filepath.Join(cfg.SafetyDir, fineTuneSafetyHold)); err != nil {
		t.Fatalf("restore failure lost safety hold: %v", err)
	}
}

type blockedFineTuneLaneHandler struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *blockedFineTuneLaneHandler) CanHandle(t string) bool { return t == JobTypeServiceStart }
func (h *blockedFineTuneLaneHandler) Execute(_ context.Context, _ *Job, _ StreamWriter) (*JobResult, error) {
	h.once.Do(func() { close(h.started) })
	<-h.release
	return &JobResult{Status: JobStatusSuccess}, nil
}

func TestFineTuneQueuedBeforeExecuteStillPreemptsBeforeDemand(t *testing.T) {
	cfg, training, _, reservation := fineTuneFixture(t)
	started := make(chan struct{})
	cfg.Run = func(ctx context.Context, _ FineTuneSpec, _ func(map[string]any) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	blocker := &blockedFineTuneLaneHandler{started: make(chan struct{}), release: make(chan struct{})}
	demand := &Job{ID: "queued-demand", Type: JobTypeShellCommand, SourceQueue: training.SourceQueue}
	source := &fineTuneYieldSource{
		MockJobSource: NewMockJobSource("redis", nil),
		jobs:          []*Job{{ID: "blocker", Type: JobTypeServiceStart}, training, demand},
		before:        map[int]<-chan struct{}{1: blocker.started},
		demand:        demand.ID,
		outcome:       make(chan string, 1),
	}
	demandHandler := NewMockJobHandler(demand.Type, false)
	runner := NewRunner(source, []JobHandler{blocker, NewFineTuneHandler(cfg), demandHandler}, RunnerConfig{
		NodeID: "1297", MaxConcurrency: 2, State: NewWorkerState(), ActivityFn: func(string, string) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	select {
	case <-source.outcome:
		t.Fatal("demand admitted while trainer had not entered Execute")
	case <-time.After(50 * time.Millisecond):
	}
	close(blocker.release)
	select {
	case outcome := <-source.outcome:
		if outcome != "admitted" || len(demandHandler.executed) != 1 || reservation.release != 1 {
			t.Fatalf("outcome=%s executed=%d restore=%d", outcome, len(demandHandler.executed), reservation.release)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued fine-tune was not safely preempted")
	}
	cancel()
	<-done
}
