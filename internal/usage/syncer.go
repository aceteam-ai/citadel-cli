package usage

import (
	"context"
	"fmt"
	"time"
)

// PayloadVersion is the wire version of the usage stream payload the publisher
// emits (cmd/work.go's usageStreamEntry). Bumped to "2.0" for aceteam#10876 C7,
// which adds work attribution (origin/org_id/work_action), typed units, and the
// receipt manifest binding to the published record.
const PayloadVersion = "2.0"

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
// On shutdown, performs a final drain to publish any remaining records.
func (s *Syncer) Start(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final drain: try to sync remaining records with a short timeout
			drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s.syncOnce(drainCtx)
			cancel()
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

// maxDrainPages bounds how many QueryUnsynced pages one syncOnce drains, so a
// backlog accumulated while offline (the "offline for an hour" case) is fully
// drained on reconnect in a single cycle rather than batchSize-per-tick over
// many minutes — while still terminating if MarkSynced ever silently no-ops.
const maxDrainPages = 10000

// syncOnce drains unsynced records to the publish target. Each record is
// published INDIVIDUALLY and marked synced immediately on success, so a failure
// partway through a page never re-sends an already-published record on the next
// cycle (exactly-once at the ledger, combined with the unique-job_id INSERT OR
// IGNORE dedup on the insert side). The first publish error ends the cycle; the
// unpublished remainder stays unsynced and is retried next cycle.
func (s *Syncer) syncOnce(ctx context.Context) {
	published := 0
	for page := 0; page < maxDrainPages; page++ {
		if ctx.Err() != nil {
			return
		}
		records, err := s.store.QueryUnsynced(s.batchSize)
		if err != nil {
			s.log("warning", fmt.Sprintf("usage sync: query failed: %v", err))
			return
		}
		if len(records) == 0 {
			break
		}
		for _, r := range records {
			if err := s.publishFn(ctx, []UsageRecord{r}); err != nil {
				s.log("warning", fmt.Sprintf("usage sync: publish failed after %d (job %s): %v", published, r.JobID, err))
				if published > 0 {
					s.log("info", fmt.Sprintf("usage sync: published %d records", published))
				}
				return
			}
			if err := s.store.MarkSynced([]int64{r.ID}); err != nil {
				s.log("warning", fmt.Sprintf("usage sync: mark synced failed (job %s): %v", r.JobID, err))
				return
			}
			published++
		}
	}
	if published > 0 {
		s.log("info", fmt.Sprintf("usage sync: published %d records", published))
	}
}

func (s *Syncer) log(level, msg string) {
	if s.logFn != nil {
		s.logFn(level, msg)
	}
}
