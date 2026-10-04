package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/finetunesafety"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

type fineTuneAcceptanceControl struct {
	mu       sync.Mutex
	updates  []map[string]any
	progress chan struct{}
	once     sync.Once
}

func (c *fineTuneAcceptanceControl) Cancelled(context.Context, string) (bool, error) {
	return false, nil
}

func (c *fineTuneAcceptanceControl) Update(_ context.Context, _ string, fields map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, fields)
	return nil
}

func (c *fineTuneAcceptanceControl) FailCritical(ctx context.Context, id string, fields map[string]any) error {
	return c.Update(ctx, id, fields)
}

func (c *fineTuneAcceptanceControl) Progress(context.Context, string, map[string]any) error {
	c.once.Do(func() { close(c.progress) })
	return nil
}

func (c *fineTuneAcceptanceControl) sawStatus(want string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, update := range c.updates {
		if update["status"] == want {
			return true
		}
	}
	return false
}

// TestFineTuneRTXAcceptance is opt-in because it requires Docker, an NVIDIA
// GPU, the local training image, a populated Hugging Face cache, and a real
// Citadel node configuration. It exercises the same handler and reservation
// implementation used by the worker, including success, demand preemption,
// verified container termination, service restoration, and safety-hold cleanup.
func TestFineTuneRTXAcceptance(t *testing.T) {
	if os.Getenv("CITADEL_FINETUNE_ACCEPTANCE") != "1" {
		t.Skip("set CITADEL_FINETUNE_ACCEPTANCE=1 on an approved GPU node")
	}
	configDir := os.Getenv("CITADEL_FINETUNE_CONFIG_DIR")
	cacheDir := os.Getenv("CITADEL_FINETUNE_CACHE_DIR")
	if configDir == "" || cacheDir == "" {
		t.Fatal("CITADEL_FINETUNE_CONFIG_DIR and CITADEL_FINETUNE_CACHE_DIR are required")
	}
	if _, err := os.Stat(filepath.Join(configDir, "citadel.yaml")); err != nil {
		t.Fatalf("node manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "hub", "models--Qwen--Qwen3-0.6B", "refs", "main")); err != nil {
		t.Fatalf("Qwen3-0.6B cache: %v", err)
	}
	if _, err := os.Lstat(finetunesafety.Path(configDir)); err == nil {
		t.Fatal("refusing acceptance run while a fine-tune safety hold already exists")
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect fine-tune safety hold: %v", err)
	}

	workspace := t.TempDir()
	dataset := filepath.Join(workspace, "train.jsonl")
	var rows strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&rows, "{\"text\":\"Citadel acceptance row %d: local training keeps data on this node.\"}\n", i)
	}
	if err := os.WriteFile(dataset, []byte(rows.String()), 0600); err != nil {
		t.Fatal(err)
	}

	newHandler := func(jobID string, control *fineTuneAcceptanceControl) (*worker.FineTuneHandler, *worker.Job, string) {
		outputRoot := t.TempDir()
		service := jobs.NewServiceHandlerWithWorkspace(configDir, configDir)
		handler := worker.NewFineTuneHandler(worker.FineTuneConfig{
			NodeID:       "acceptance-node",
			WorkspaceDir: workspace,
			OutputRoot:   outputRoot,
			SafetyDir:    finetunesafety.Dir(configDir),
			CacheDir:     cacheDir,
			Image:        "citadel-finetune:local",
			Control:      control,
			Reservation:  &fineTuneServiceReservation{service: service},
		})
		job := &worker.Job{
			ID:          jobID,
			Type:        worker.JobTypeFineTuneStart,
			SourceQueue: "jobs:v1:shell:org_acceptance:node:acceptance-node",
			Payload: map[string]any{
				"node_id": "acceptance-node", "dataset_node_id": "acceptance-node",
				"dataset_node_path": "train.jsonl", "model": "Qwen/Qwen3-0.6B", "method": "lora",
				"hyperparameters": map[string]any{
					"epochs": 1, "learning_rate": 0.0002, "batch_size": 1,
					"lora_r": 8, "lora_alpha": 16, "lora_dropout": 0.0, "max_seq_length": 128,
				},
			},
		}
		return handler, job, filepath.Join(outputRoot, jobID)
	}

	t.Run("success produces adapter and restores services", func(t *testing.T) {
		control := &fineTuneAcceptanceControl{progress: make(chan struct{})}
		handler, job, outputDir := newHandler("accept-success", control)
		result, err := handler.Execute(context.Background(), job, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != worker.JobStatusSuccess || !control.sawStatus("succeeded") {
			t.Fatalf("result=%+v updates=%+v", result, control.updates)
		}
		adapter := filepath.Join(outputDir, "adapter_model.safetensors")
		if info, err := os.Stat(adapter); err != nil || info.Size() == 0 {
			t.Fatalf("adapter artifact %s: info=%v err=%v", adapter, info, err)
		}
	})

	t.Run("demand preempts container and restores services", func(t *testing.T) {
		control := &fineTuneAcceptanceControl{progress: make(chan struct{})}
		handler, job, _ := newHandler("accept-preempt", control)
		resultCh := make(chan *worker.JobResult, 1)
		errCh := make(chan error, 1)
		go func() {
			result, err := handler.Execute(context.Background(), job, nil)
			resultCh <- result
			errCh <- err
		}()
		select {
		case <-control.progress:
		case <-time.After(2 * time.Minute):
			t.Fatal("training did not emit progress")
		}
		if err := handler.YieldToDemand(); err != nil {
			t.Fatalf("demand was not admitted after verified cleanup: %v", err)
		}
		select {
		case result := <-resultCh:
			if err := <-errCh; err != nil {
				t.Fatal(err)
			}
			if result.Status != worker.JobStatusCancelled || !control.sawStatus("cancelled") {
				t.Fatalf("result=%+v updates=%+v", result, control.updates)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("preempted fine-tune did not terminate")
		}
	})

	if _, err := os.Lstat(finetunesafety.Path(configDir)); err == nil {
		t.Fatal("fine-tune safety hold remained after acceptance")
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect final fine-tune safety hold: %v", err)
	}
}
