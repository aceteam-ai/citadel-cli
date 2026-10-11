package usage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func seedRecords(t *testing.T, store *Store, count int) {
	t.Helper()
	now := time.Now().UTC()
	for i := range count {
		if err := store.Insert(UsageRecord{
			JobID:       fmt.Sprintf("sync-job-%d", i),
			JobType:     "llm_inference",
			Status:      "success",
			StartedAt:   now,
			CompletedAt: now.Add(time.Second),
			DurationMs:  1000,
			NodeID:      "test-node",
		}); err != nil {
			t.Fatalf("seed Insert: %v", err)
		}
	}
}

func TestSyncerPublishesAndMarksSynced(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	seedRecords(t, store, 3)

	var published []UsageRecord
	var mu sync.Mutex

	syncer := NewSyncer(SyncerConfig{
		Store:     store,
		BatchSize: 10,
		PublishFn: func(ctx context.Context, records []UsageRecord) error {
			mu.Lock()
			published = append(published, records...)
			mu.Unlock()
			return nil
		},
	})

	syncer.SyncOnce(context.Background())

	mu.Lock()
	publishedCount := len(published)
	mu.Unlock()

	if publishedCount != 3 {
		t.Errorf("expected 3 published records, got %d", publishedCount)
	}

	// All should now be synced
	remaining, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected 0 unsynced after sync, got %d", len(remaining))
	}
}

func TestSyncerPublishFailureDoesNotMarkSynced(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	seedRecords(t, store, 2)

	syncer := NewSyncer(SyncerConfig{
		Store:     store,
		BatchSize: 10,
		PublishFn: func(ctx context.Context, records []UsageRecord) error {
			return errors.New("connection refused")
		},
	})

	syncer.SyncOnce(context.Background())

	// Records should still be unsynced
	remaining, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	if len(remaining) != 2 {
		t.Errorf("expected 2 unsynced after failed publish, got %d", len(remaining))
	}
}

// TestSyncerDrainsAllInOneCycle pins the aceteam#10876 C7 contract: one SyncOnce
// drains the ENTIRE backlog (the "offline for an hour, reconnect" case), one
// record per publish call, regardless of the QueryUnsynced page size (BatchSize).
func TestSyncerDrainsAllInOneCycle(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	seedRecords(t, store, 5)

	var publishCalls int
	var mu sync.Mutex

	syncer := NewSyncer(SyncerConfig{
		Store:     store,
		BatchSize: 2, // page size; not the publish granularity
		PublishFn: func(ctx context.Context, records []UsageRecord) error {
			mu.Lock()
			publishCalls++
			mu.Unlock()
			if len(records) != 1 {
				t.Errorf("publish granularity = %d, want 1 (per-record)", len(records))
			}
			return nil
		},
	})

	// A single SyncOnce drains all 5 across pages of 2.
	syncer.SyncOnce(context.Background())

	mu.Lock()
	if publishCalls != 5 {
		t.Errorf("publish calls = %d, want 5 (all drained in one cycle)", publishCalls)
	}
	mu.Unlock()

	remaining, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected 0 unsynced after one drain cycle, got %d", len(remaining))
	}
}

// TestSyncerOutboxReplayExactlyOnce pins the acceptance: a node offline for a
// while buffers records; on reconnect ALL drain, and a subsequent cycle sends
// nothing (exactly once).
func TestSyncerOutboxReplayExactlyOnce(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	seedRecords(t, store, 7)

	var mu sync.Mutex
	published := map[string]int{} // job_id -> times published
	online := false

	syncer := NewSyncer(SyncerConfig{
		Store:     store,
		BatchSize: 3,
		PublishFn: func(ctx context.Context, records []UsageRecord) error {
			mu.Lock()
			defer mu.Unlock()
			if !online {
				return errors.New("offline")
			}
			for _, r := range records {
				published[r.JobID]++
			}
			return nil
		},
	})

	// Offline: nothing drains, everything stays buffered.
	syncer.SyncOnce(context.Background())
	if n := countUnsynced(t, store); n != 7 {
		t.Fatalf("offline: expected 7 buffered, got %d", n)
	}

	// Reconnect: one cycle drains all 7.
	mu.Lock()
	online = true
	mu.Unlock()
	syncer.SyncOnce(context.Background())
	if n := countUnsynced(t, store); n != 0 {
		t.Fatalf("after reconnect: expected 0 buffered, got %d", n)
	}

	// A second cycle sends nothing more.
	syncer.SyncOnce(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(published) != 7 {
		t.Fatalf("expected 7 distinct records published, got %d", len(published))
	}
	for id, n := range published {
		if n != 1 {
			t.Errorf("record %s published %d times, want exactly 1", id, n)
		}
	}
}

// TestSyncerPartialFailureNoDoubleSend pins that a publish error partway through
// marks only the records published BEFORE it, and a retry does not re-send them.
func TestSyncerPartialFailureNoDoubleSend(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	seedRecords(t, store, 5)

	var mu sync.Mutex
	published := map[string]int{}
	failAfter := 2 // first 2 succeed, the 3rd fails
	count := 0

	syncer := NewSyncer(SyncerConfig{
		Store:     store,
		BatchSize: 10,
		PublishFn: func(ctx context.Context, records []UsageRecord) error {
			mu.Lock()
			defer mu.Unlock()
			if count >= failAfter {
				return errors.New("boom")
			}
			count++
			for _, r := range records {
				published[r.JobID]++
			}
			return nil
		},
	})

	// First cycle: 2 succeed, 3rd fails, cycle ends. 3 remain unsynced.
	syncer.SyncOnce(context.Background())
	if n := countUnsynced(t, store); n != 3 {
		t.Fatalf("after partial failure: expected 3 unsynced, got %d", n)
	}

	// Recover: now everything publishes.
	mu.Lock()
	failAfter = 1 << 30
	mu.Unlock()
	syncer.SyncOnce(context.Background())
	if n := countUnsynced(t, store); n != 0 {
		t.Fatalf("after recovery: expected 0 unsynced, got %d", n)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(published) != 5 {
		t.Fatalf("expected 5 distinct records, got %d", len(published))
	}
	for id, n := range published {
		if n != 1 {
			t.Errorf("record %s published %d times, want exactly 1 (no double-send)", id, n)
		}
	}
}

func countUnsynced(t *testing.T, store *Store) int {
	t.Helper()
	recs, err := store.QueryUnsynced(1000)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	return len(recs)
}

func TestSyncerNoRecordsIsNoop(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	publishCalled := false
	syncer := NewSyncer(SyncerConfig{
		Store:     store,
		BatchSize: 10,
		PublishFn: func(ctx context.Context, records []UsageRecord) error {
			publishCalled = true
			return nil
		},
	})

	syncer.SyncOnce(context.Background())

	if publishCalled {
		t.Error("publishFn should not be called when there are no records")
	}
}

func TestSyncerStartRespectsContext(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	syncer := NewSyncer(SyncerConfig{
		Store:     store,
		Interval:  10 * time.Millisecond,
		BatchSize: 10,
		PublishFn: func(ctx context.Context, records []UsageRecord) error {
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err = syncer.Start(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Start should return context.DeadlineExceeded, got %v", err)
	}
}

func TestSyncerLogFn(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	seedRecords(t, store, 1)

	var logMessages []string
	var mu sync.Mutex

	syncer := NewSyncer(SyncerConfig{
		Store:     store,
		BatchSize: 10,
		PublishFn: func(ctx context.Context, records []UsageRecord) error {
			return nil
		},
		LogFn: func(level, msg string) {
			mu.Lock()
			logMessages = append(logMessages, msg)
			mu.Unlock()
		},
	})

	syncer.SyncOnce(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(logMessages) == 0 {
		t.Error("expected log messages from successful sync")
	}
}
