package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "usage_test.db")
}

func TestOpenStore(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
}

func TestOpenStoreCreatesFile(t *testing.T) {
	path := tempDBPath(t)
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("database file should exist after OpenStore")
	}
}

func TestInsertAndQueryUnsynced(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Second)
	record := UsageRecord{
		JobID:            "job-001",
		JobType:          "llm_inference",
		Backend:          "vllm",
		Model:            "meta-llama/Llama-2-7b",
		Status:           "success",
		StartedAt:        now,
		CompletedAt:      now.Add(3 * time.Second),
		DurationMs:       3000,
		PromptTokens:     128,
		CompletionTokens: 256,
		TotalTokens:      384,
		RequestBytes:     1024,
		ResponseBytes:    4096,
		NodeID:           "test-node",
	}

	if err := store.Insert(record); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	records, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}

	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	r := records[0]
	if r.JobID != "job-001" {
		t.Errorf("JobID = %q, want %q", r.JobID, "job-001")
	}
	if r.Backend != "vllm" {
		t.Errorf("Backend = %q, want %q", r.Backend, "vllm")
	}
	if r.DurationMs != 3000 {
		t.Errorf("DurationMs = %d, want 3000", r.DurationMs)
	}
	if r.PromptTokens != 128 {
		t.Errorf("PromptTokens = %d, want 128", r.PromptTokens)
	}
	if r.TotalTokens != 384 {
		t.Errorf("TotalTokens = %d, want 384", r.TotalTokens)
	}
	if r.NodeID != "test-node" {
		t.Errorf("NodeID = %q, want %q", r.NodeID, "test-node")
	}
	if r.ID == 0 {
		t.Error("ID should be set after insert")
	}
}

func TestInsertDuplicateIgnored(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	record := UsageRecord{
		JobID:       "dup-job",
		JobType:     "test",
		Status:      "success",
		StartedAt:   now,
		CompletedAt: now,
		DurationMs:  100,
		NodeID:      "node1",
	}

	if err := store.Insert(record); err != nil {
		t.Fatalf("first Insert: %v", err)
	}

	// Second insert with same job_id should not error
	if err := store.Insert(record); err != nil {
		t.Fatalf("duplicate Insert should not error: %v", err)
	}

	records, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("expected 1 record after duplicate insert, got %d", len(records))
	}
}

func TestMarkSynced(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	for i, id := range []string{"a", "b", "c"} {
		if err := store.Insert(UsageRecord{
			JobID:       id,
			JobType:     "test",
			Status:      "success",
			StartedAt:   now,
			CompletedAt: now.Add(time.Duration(i) * time.Second),
			DurationMs:  int64(i * 1000),
			NodeID:      "node1",
		}); err != nil {
			t.Fatalf("Insert %s: %v", id, err)
		}
	}

	// Query all unsynced
	records, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("expected 3 unsynced, got %d", len(records))
	}

	// Mark first two as synced
	if err := store.MarkSynced([]int64{records[0].ID, records[1].ID}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	// Only one should remain unsynced
	remaining, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced after mark: %v", err)
	}
	if len(remaining) != 1 {
		t.Errorf("expected 1 remaining unsynced, got %d", len(remaining))
	}
	if remaining[0].JobID != "c" {
		t.Errorf("remaining JobID = %q, want %q", remaining[0].JobID, "c")
	}
}

func TestMarkSyncedEmpty(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// Should not error with empty slice
	if err := store.MarkSynced(nil); err != nil {
		t.Fatalf("MarkSynced(nil): %v", err)
	}
	if err := store.MarkSynced([]int64{}); err != nil {
		t.Fatalf("MarkSynced([]): %v", err)
	}
}

func TestQueryUnsyncedLimit(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	for i := range 5 {
		if err := store.Insert(UsageRecord{
			JobID:       fmt.Sprintf("job-%d", i),
			JobType:     "test",
			Status:      "success",
			StartedAt:   now,
			CompletedAt: now,
			DurationMs:  100,
			NodeID:      "node1",
		}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	records, err := store.QueryUnsynced(2)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	if len(records) != 2 {
		t.Errorf("expected 2 records with limit=2, got %d", len(records))
	}
}

func TestInsertWithErrorMessage(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	record := UsageRecord{
		JobID:        "fail-job",
		JobType:      "llm_inference",
		Status:       "failed",
		StartedAt:    now,
		CompletedAt:  now,
		DurationMs:   50,
		ErrorMessage: "out of memory",
		NodeID:       "node1",
	}

	if err := store.Insert(record); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	records, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	if records[0].ErrorMessage != "out of memory" {
		t.Errorf("ErrorMessage = %q, want %q", records[0].ErrorMessage, "out of memory")
	}
}
