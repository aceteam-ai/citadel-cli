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

func TestSyncerBatchSize(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	seedRecords(t, store, 5)

	var batchSizes []int
	var mu sync.Mutex

	syncer := NewSyncer(SyncerConfig{
		Store:     store,
		BatchSize: 2,
		PublishFn: func(ctx context.Context, records []UsageRecord) error {
			mu.Lock()
			batchSizes = append(batchSizes, len(records))
			mu.Unlock()
			return nil
		},
	})

	// First sync: should get batch of 2
	syncer.SyncOnce(context.Background())

	mu.Lock()
	if len(batchSizes) != 1 || batchSizes[0] != 2 {
		t.Errorf("first batch size = %v, want [2]", batchSizes)
	}
	mu.Unlock()

	// Second sync: next batch of 2
	syncer.SyncOnce(context.Background())

	// Third sync: last 1
	syncer.SyncOnce(context.Background())

	mu.Lock()
	if len(batchSizes) != 3 {
		t.Errorf("expected 3 sync cycles, got %d", len(batchSizes))
	}
	if batchSizes[2] != 1 {
		t.Errorf("third batch size = %d, want 1", batchSizes[2])
	}
	mu.Unlock()
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
