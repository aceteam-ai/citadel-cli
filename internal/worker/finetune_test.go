package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
)

type fineTuneControlFake struct {
	mu        sync.Mutex
	cancelled bool
	updates   []map[string]any
	critical  []map[string]any
	events    []map[string]any
	updateErr error
}

func (c *fineTuneControlFake) Cancelled(context.Context, string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancelled, nil
}
func (c *fineTuneControlFake) Update(_ context.Context, _ string, fields map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.updateErr != nil {
		return c.updateErr
	}
	c.updates = append(c.updates, fields)
	return nil
}
func (c *fineTuneControlFake) FailCritical(_ context.Context, _ string, fields map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.updateErr != nil {
		return c.updateErr
	}
	c.critical = append(c.critical, fields)
	c.updates = append(c.updates, fields)
	return nil
}
func (c *fineTuneControlFake) Progress(_ context.Context, _ string, event map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
	return nil
}

type fineTuneReservationFake struct {
	mu               sync.Mutex
	reserve, release int
	releaseErr       error
}

func (r *fineTuneReservationFake) Reserve(context.Context, string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reserve++
	return []string{"unlimited-ocr", "ollama"}, nil
}
func (r *fineTuneReservationFake) Release(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.release++
	return r.releaseErr
}

func fineTuneFixture(t *testing.T) (FineTuneConfig, *Job, *fineTuneControlFake, *fineTuneReservationFake) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "train.jsonl"), []byte("{\"text\":\"hello\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	control := &fineTuneControlFake{}
	reservation := &fineTuneReservationFake{}
	cfg := FineTuneConfig{NodeID: "1297", WorkspaceDir: workspace, OutputRoot: filepath.Join(t.TempDir(), "adapters"), CacheDir: filepath.Join(t.TempDir(), "cache"), Control: control, Reservation: reservation}
	job := &Job{ID: "job-1", Type: JobTypeFineTuneStart, SourceQueue: "jobs:v1:shell:org_o:node:1297", Payload: map[string]any{
		"node_id": "1297", "dataset_node_id": "1297", "dataset_node_path": "train.jsonl", "model": "Qwen/Qwen3-0.6B", "method": "qlora",
		"hyperparameters": map[string]any{"epochs": 2, "learning_rate": 0.0002, "batch_size": 4, "lora_r": 16, "lora_alpha": 32, "lora_dropout": 0.05, "max_seq_length": 2048},
	}}
	return cfg, job, control, reservation
}

func TestFineTuneRejectsMissingPathAndOffAllowlistModel(t *testing.T) {
	for _, mutate := range []struct {
		name  string
		apply func(*Job)
	}{
		{"missing dataset", func(j *Job) { delete(j.Payload, "dataset_node_path") }},
		{"off allowlist", func(j *Job) { j.Payload["model"] = "other/model" }},
		{"URL source", func(j *Job) { j.Payload["dataset_url"] = "https://example.com/train.jsonl" }},
		{"wrong node", func(j *Job) { j.Payload["dataset_node_id"] = "peer" }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			cfg, job, _, reservation := fineTuneFixture(t)
			mutate.apply(job)
			res, _ := NewFineTuneHandler(cfg).Execute(context.Background(), job, &MockStreamWriter{})
			if res.Status != JobStatusTerminalFailure {
				t.Fatalf("status = %q, want terminal failure", res.Status)
			}
			if reservation.reserve != 0 {
				t.Fatal("invalid payload reached preemption")
			}
		})
	}
}

func TestFineTuneRejectsSymlinkEscape(t *testing.T) {
	cfg, job, _, _ := fineTuneFixture(t)
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cfg.WorkspaceDir, "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	job.Payload["dataset_node_path"] = "link.jsonl"
	if _, err := parseFineTune(job, cfg.WorkspaceDir, cfg.OutputRoot); err == nil {
		t.Fatal("symlink escape accepted")
	}
}

func TestFineTuneProgressAndFailureRestore(t *testing.T) {
	cfg, job, control, reservation := fineTuneFixture(t)
	cfg.Run = func(_ context.Context, _ FineTuneSpec, progress func(map[string]any) error) error {
		if err := progress(map[string]any{"progress_percent": 50.0, "current_epoch": 1.0, "current_loss": 0.7}); err != nil {
			return err
		}
		return errors.New("training failed")
	}
	res, _ := NewFineTuneHandler(cfg).Execute(context.Background(), job, &MockStreamWriter{})
	if res.Status != JobStatusTerminalFailure {
		t.Fatalf("status = %q", res.Status)
	}
	if reservation.reserve != 1 || reservation.release != 1 {
		t.Fatalf("reservation = %d/%d", reservation.reserve, reservation.release)
	}
	if len(control.events) != 1 || control.events[0]["progress_percent"] != 50.0 || control.events[0]["current_epoch"] != 1.0 || control.events[0]["current_loss"] != 0.7 {
		t.Fatalf("progress = %#v", control.events)
	}
	if control.updates[len(control.updates)-1]["status"] != "failed" {
		t.Fatalf("final status = %#v", control.updates)
	}
}

func TestFineTuneRejectsInjectedProgressAndRestores(t *testing.T) {
	cfg, job, control, reservation := fineTuneFixture(t)
	cfg.Run = func(_ context.Context, _ FineTuneSpec, progress func(map[string]any) error) error {
		return progress(map[string]any{"progress_percent": 50.0, "current_epoch": 1.0, "current_loss": 0.7, "status": "succeeded"})
	}
	res, _ := NewFineTuneHandler(cfg).Execute(context.Background(), job, &MockStreamWriter{})
	if res.Status != JobStatusTerminalFailure || reservation.release != 1 || len(control.events) != 0 {
		t.Fatalf("result=%+v restore=%d events=%v", res, reservation.release, control.events)
	}
}

func TestFineTuneDemandKillsRunAndRestoresBeforeReturning(t *testing.T) {
	cfg, job, control, reservation := fineTuneFixture(t)
	started := make(chan struct{})
	stopped := make(chan struct{})
	cfg.Run = func(ctx context.Context, _ FineTuneSpec, _ func(map[string]any) error) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}
	h := NewFineTuneHandler(cfg)
	stream := &MockStreamWriter{}
	result := make(chan *JobResult, 1)
	go func() { res, _ := h.Execute(context.Background(), job, stream); result <- res }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("training never started")
	}
	h.YieldToDemand()
	select {
	case <-stopped:
	default:
		t.Fatal("child did not observe cancellation")
	}
	select {
	case res := <-result:
		if res.Status != JobStatusCancelled {
			t.Fatalf("status = %q", res.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("training did not finish")
	}
	if reservation.release != 1 || !stream.cancelled {
		t.Fatalf("restore=%d cancelled=%v", reservation.release, stream.cancelled)
	}
	if control.updates[len(control.updates)-1]["status"] != "cancelled" {
		t.Fatalf("final status = %#v", control.updates)
	}
}

func TestFineTuneCancelKeyKillsRunAndRestores(t *testing.T) {
	cfg, job, control, reservation := fineTuneFixture(t)
	started := make(chan struct{})
	cfg.Run = func(ctx context.Context, _ FineTuneSpec, _ func(map[string]any) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	h := NewFineTuneHandler(cfg)
	result := make(chan *JobResult, 1)
	go func() { res, _ := h.Execute(context.Background(), job, &MockStreamWriter{}); result <- res }()
	<-started
	control.mu.Lock()
	control.cancelled = true
	control.mu.Unlock()
	select {
	case res := <-result:
		if res.Status != JobStatusCancelled {
			t.Fatalf("status = %q", res.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel key was not honored")
	}
	if reservation.release != 1 {
		t.Fatalf("restore=%d", reservation.release)
	}
}

func TestFineTuneRemovalFailureKeepsReservationAndNeverReportsCancelled(t *testing.T) {
	cfg, job, control, reservation := fineTuneFixture(t)
	engine := filepath.Join(t.TempDir(), "fake-engine")
	script := "#!/bin/sh\ncase \"$1\" in\nrun) exec sleep 30 ;;\nrm) exit 42 ;;\nps) printf 'citadel-finetune-job-1\\n' ;;\nesac\n"
	if err := os.WriteFile(engine, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Run = func(ctx context.Context, spec FineTuneSpec, progress func(map[string]any) error) error {
		cancelledCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		return runFineTuneContainerWithRuntime(cancelledCtx, spec, "fake-image", cfg.CacheDir,
			catalog.ContainerRuntime{EngineBin: engine}, progress)
	}
	// Mark the job cancelled when the fake child returns. The pre-start check
	// must first be allowed through so the removal path is actually exercised.
	originalRun := cfg.Run
	cfg.Run = func(ctx context.Context, spec FineTuneSpec, progress func(map[string]any) error) error {
		err := originalRun(ctx, spec, progress)
		control.mu.Lock()
		control.cancelled = true
		control.mu.Unlock()
		return err
	}
	stream := &MockStreamWriter{}
	res, _ := NewFineTuneHandler(cfg).Execute(context.Background(), job, stream)
	if res.Status != JobStatusTerminalFailure || res.Error == nil || !strings.Contains(res.Error.Error(), "termination unconfirmed") {
		t.Fatalf("result=%+v", res)
	}
	if reservation.release != 0 || stream.cancelled {
		t.Fatalf("unsafe restore=%d cancelled=%v", reservation.release, stream.cancelled)
	}
	if len(control.critical) != 1 || control.critical[0]["status"] != "failed" {
		t.Fatalf("critical failure not persisted: %#v", control.critical)
	}
}

func TestFineTuneRestoreFailureUnderUserCancelIsReported(t *testing.T) {
	cfg, job, control, reservation := fineTuneFixture(t)
	reservation.releaseErr = errors.New("restore failed")
	cfg.Run = func(context.Context, FineTuneSpec, func(map[string]any) error) error {
		control.mu.Lock()
		control.cancelled = true
		control.mu.Unlock()
		return context.Canceled
	}
	stream := &MockStreamWriter{}
	res, _ := NewFineTuneHandler(cfg).Execute(context.Background(), job, stream)
	if res.Status != JobStatusTerminalFailure || res.Error == nil || !strings.Contains(res.Error.Error(), "restore failed") {
		t.Fatalf("result=%+v", res)
	}
	if reservation.release != 1 || stream.cancelled {
		t.Fatalf("restore attempts=%d cancelled=%v", reservation.release, stream.cancelled)
	}
	if len(control.critical) != 1 || control.critical[0]["status"] != "failed" {
		t.Fatalf("restore failure not persisted: %#v", control.critical)
	}
}

func TestFineTuneCancellationUpdateFailureDoesNotClaimCancelled(t *testing.T) {
	cfg, job, control, reservation := fineTuneFixture(t)
	stream := &MockStreamWriter{}
	control.cancelled = true
	control.updateErr = errors.New("status store unavailable")
	res, _ := NewFineTuneHandler(cfg).Execute(context.Background(), job, stream)
	if res.Status != JobStatusTerminalFailure || res.Error == nil || !strings.Contains(res.Error.Error(), "canonical status update failed") {
		t.Fatalf("result=%+v", res)
	}
	if stream.cancelled || reservation.reserve != 0 {
		t.Fatalf("claimed cancellation before persistence: stream=%v reserve=%d", stream.cancelled, reservation.reserve)
	}
}

func TestFineTuneDockerContract(t *testing.T) {
	spec := FineTuneSpec{JobID: "job-1", DatasetPath: "/node/work/train.jsonl", OutputDir: "/node/adapters/job-1"}
	args, err := fineTuneDockerArgs(spec, "citadel-finetune:local", "/node/cache")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"run --rm -i", "--gpus all", "--network none", "--read-only", "dst=/data/train.jsonl,readonly", "dst=/output", "dst=/cache,readonly", "HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1", "citadel-finetune:local"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Docker arguments lack %q: %s", want, joined)
		}
	}
	if _, err := fineTuneDockerArgs(FineTuneSpec{DatasetPath: "/bad,source", OutputDir: spec.OutputDir}, "citadel-finetune:local", "/node/cache"); err == nil {
		t.Fatal("Docker mount option injection accepted")
	}
}
