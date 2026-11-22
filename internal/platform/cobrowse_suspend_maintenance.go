package platform

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	suspendMaintenanceInterval = time.Hour
	suspendMaintenanceCatchup  = time.Second
	suspendMaintenanceBudget   = 5 * time.Second
)

type suspendMaintenanceState struct {
	mu                      sync.Mutex
	running                 bool
	ttlCursor, orphanCursor string
}

// RunSuspendHousekeeping is a managed-worker lifecycle, not a browser action.
// It expires tab payloads and prunes metadata/orphans only. It never launches,
// stops or adopts a browser, nor opens/deletes a saved profile. No new registry
// or payload directory is created. Each pass retains process/store ownership
// and registry exclusion; errors do not authorize deletion by another path.
func (m *CobrowseSessionManager) RunSuspendHousekeeping(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil || !m.useResolver {
		return errors.New("browser housekeeping requires a running managed worker")
	}
	m.mu.Lock()
	if m.suspendMaintenance.running {
		m.mu.Unlock()
		return errors.New("browser housekeeping is already running")
	}
	m.suspendMaintenance.running = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.suspendMaintenance.running = false
		m.mu.Unlock()
	}()
	return runSuspendHousekeeping(ctx, time.Now, waitSuspendHousekeeping, m.sweepSuspendMetadata,
		func() { log.Print("[cobrowse] suspended metadata housekeeping incomplete; retry scheduled") })
}

func waitSuspendHousekeeping(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

// A successful full batch gets a bounded follow-up; any error backs off to the
// ordinary cadence rather than hammering a bad filesystem. Every pass shares
// the worker lifetime and one finite budget. Providers must honor that budget.
func runSuspendHousekeeping(ctx context.Context, now func() time.Time,
	wait func(context.Context, time.Duration) bool,
	pass func(context.Context, time.Time) (bool, error), report func()) error {
	if ctx == nil || now == nil || wait == nil || pass == nil || report == nil {
		return errors.New("browser housekeeping dependencies unavailable")
	}
	for ctx.Err() == nil {
		budget, cancel := context.WithTimeout(ctx, suspendMaintenanceBudget)
		more, err := pass(budget, now())
		budgetErr := budget.Err()
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		delay := suspendMaintenanceInterval
		if err != nil || budgetErr != nil {
			report()
		} else if more {
			delay = suspendMaintenanceCatchup
		}
		if !wait(ctx, delay) {
			return nil
		}
	}
	return nil
}

// Metadata effects use the same state directory as status, but a separate
// non-creating open. Cursor progress survives partial errors. The two cleanup
// passes independently revalidate all retained references before any removal.
func (m *CobrowseSessionManager) sweepSuspendMetadata(ctx context.Context, now time.Time) (bool, error) {
	return m.sweepSuspendMetadataWith(ctx, now, func(dir string) (*suspendIntentStore, error) {
		return openSuspendIntentStoreWithCreate(dir, false)
	})
}

func (m *CobrowseSessionManager) sweepSuspendMetadataWith(ctx context.Context, now time.Time,
	open func(string) (*suspendIntentStore, error)) (bool, error) {
	fail := func() (bool, error) { return false, errors.New("browser metadata cleanup incomplete") }
	if ctx == nil || !m.useResolver || now.IsZero() || ctx.Err() != nil || open == nil {
		return fail()
	}
	if _, finite := ctx.Deadline(); !finite {
		return fail()
	}
	if !m.suspendMaintenance.mu.TryLock() {
		return fail()
	}
	defer m.suspendMaintenance.mu.Unlock()
	dir := m.registryDir()
	if _, err := os.Lstat(filepath.Join(dir, suspendManifestDirName)); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return fail()
	}
	store, err := open(dir)
	if err != nil || store == nil {
		return fail()
	}
	more := false
	err = store.withLifecycleExclusive(ctx, func(ctx context.Context) error {
		ttl, ttlErr := store.sweepTTL(ctx, now, m.suspendMaintenance.ttlCursor)
		m.suspendMaintenance.ttlCursor = ttl.Cursor
		orphans, orphanErr := store.sweepOrphanPayloads(ctx, m.suspendMaintenance.orphanCursor)
		m.suspendMaintenance.orphanCursor = orphans.Cursor
		more = ttl.Attempted == maxSuspendTTLTransitions || orphans.Attempted == maxSuspendOrphanRemovals
		return errors.Join(ttlErr, orphanErr)
	})
	closeErr := store.close()
	if err != nil || closeErr != nil || ctx.Err() != nil {
		return fail()
	}
	return more, nil
}
