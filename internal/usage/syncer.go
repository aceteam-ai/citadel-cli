package usage

import (
	"context"
	"fmt"
	"time"
)

// PublishFunc sends a batch of usage records to an external system (e.g., Redis).
// It should return an error if the publish fails.
type PublishFunc func(ctx context.Context, records []UsageRecord) error

// SyncerConfig holds configuration for the background syncer.
type SyncerConfig struct {
	// Store is the local usage database
	Store *Store

	// PublishFn sends records to the external system
	PublishFn PublishFunc

	// Interval between sync cycles (default: 60s)
	Interval time.Duration

	// BatchSize is the max records per sync cycle (default: 50)
	BatchSize int

	// LogFn is called for log messages (optional)
	LogFn func(level, msg string)
}

// Syncer periodically syncs unsynced usage records to an external system.
type Syncer struct {
	store     *Store
	publishFn PublishFunc
	interval  time.Duration
	batchSize int
	logFn     func(level, msg string)
}

// NewSyncer creates a new usage syncer.
func NewSyncer(cfg SyncerConfig) *Syncer {
	interval := cfg.Interval
	if interval == 0 {
		interval = 60 * time.Second
	}
	batchSize := cfg.BatchSize
	if batchSize == 0 {
		batchSize = 50
	}
	return &Syncer{
		store:     cfg.Store,
		publishFn: cfg.PublishFn,
		interval:  interval,
		batchSize: batchSize,
		logFn:     cfg.LogFn,
	}
}

// Start runs the sync loop until the context is cancelled.
func (s *Syncer) Start(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.syncOnce(ctx)
		}
	}
}

// SyncOnce performs a single sync cycle. Exported for testing.
func (s *Syncer) SyncOnce(ctx context.Context) {
	s.syncOnce(ctx)
}

func (s *Syncer) syncOnce(ctx context.Context) {
	records, err := s.store.QueryUnsynced(s.batchSize)
	if err != nil {
		s.log("warning", fmt.Sprintf("usage sync: query failed: %v", err))
		return
	}
	if len(records) == 0 {
		return
	}

	if err := s.publishFn(ctx, records); err != nil {
		s.log("warning", fmt.Sprintf("usage sync: publish failed (%d records): %v", len(records), err))
		return
	}

	ids := make([]int64, len(records))
	for i, r := range records {
		ids[i] = r.ID
	}
	if err := s.store.MarkSynced(ids); err != nil {
		s.log("warning", fmt.Sprintf("usage sync: mark synced failed: %v", err))
		return
	}

	s.log("info", fmt.Sprintf("usage sync: published %d records", len(records)))
}

func (s *Syncer) log(level, msg string) {
	if s.logFn != nil {
		s.logFn(level, msg)
	}
}
