package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
)

func TestTUIWorkerAndWorkStartupShareExclusiveLock(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")
	tuiLock, monitor, _, err := acquireTUIWorkerOwnership(stateDir)
	if err != nil || monitor || tuiLock == nil {
		t.Fatalf("TUI ownership = (lock %v, monitor %v, err %v)", tuiLock, monitor, err)
	}
	if workLock, err := worklock.Acquire(stateDir, Version, nil); err == nil {
		workLock.Release()
		t.Fatal("citadel work acquired lock while TUI worker owns the queue")
	} else {
		var running *worklock.ErrAlreadyRunning
		if !errors.As(err, &running) {
			t.Fatalf("work lock contention = %v, want already running", err)
		}
	}
	tuiLock.Release()

	workLock, err := worklock.Acquire(stateDir, Version, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer workLock.Release()
	secondTUI, monitor, _, err := acquireTUIWorkerOwnership(stateDir)
	if err != nil || !monitor || secondTUI != nil {
		t.Fatalf("TUI under work ownership = (lock %v, monitor %v, err %v)", secondTUI, monitor, err)
	}

	// A crashed TUI releases its process lock, but its training container and
	// reservation may survive. This is the exact config directory and guarded
	// reconcile call used by runWork after it obtains the lock.
	configDir := filepath.Dir(stateDir)
	manifest := []byte("services:\n  - name: unlimited-ocr\n    type: docker\n    desired_status: stopped\n    evicted_by_job: train-job\n")
	manifestPath := filepath.Join(configDir, "citadel.yaml")
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	safetyDir := filepath.Join(configDir, "finetune", "safety")
	if err := os.MkdirAll(safetyDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(safetyDir, "active.hold"), []byte("train-job\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := jobs.NewServiceHandler(configDir)
	if restored, err := h.ReconcileOrphanedReservations(jobs.JobContext{}, true); err == nil || len(restored) != 0 {
		t.Fatalf("runWork startup reconcile with trainer hold = (%v, %v)", restored, err)
	}
	got, err := os.ReadFile(manifestPath)
	if err != nil || string(got) != string(manifest) {
		t.Fatalf("startup changed reserved manifest: %q, %v", got, err)
	}
}

func TestTUIWorkerRefusesWhenOwnershipCannotBeEstablished(t *testing.T) {
	blockedDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedDir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, monitor, _, err := acquireTUIWorkerOwnership(filepath.Join(blockedDir, "network"))
	if err == nil || lock != nil || monitor {
		t.Fatalf("lock failure = (lock %v, monitor %v, err %v), want refusal", lock, monitor, err)
	}
}
