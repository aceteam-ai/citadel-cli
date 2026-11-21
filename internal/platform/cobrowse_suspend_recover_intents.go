package platform

// Unwired startup reconciliation for captures whose suspending intent was
// durably published. This never launches a browser or opens a saved profile.
// The eventual manager must exclude legacy writers for the whole pass and
// supply an idempotent, exact-incarnation stop-and-verify adapter.

import (
	"context"
	"errors"
	"sort"
	"time"
)

const maxSuspendRecoveryTransitions = 16

// Metadata-only, level-triggered progress. Completed includes an idempotent
// durability reconciliation, not a claim that a new teardown happened. Keep
// Cursor even when a partial error is returned, so broken entries cannot
// monopolize later passes. Terminated requires durable capture absence and
// verified teardown, not a provider error. Orphan cleanup is separate.
type suspendIntentRecoveryResult struct {
	Scanned, Attempted, Completed, Terminated, Failed int
	Cursor                                            string
}

func (s *suspendIntentStore) recoverSuspending(ctx context.Context, now time.Time, cursor string,
	stopAndVerify func(context.Context, suspendStopIdentity) error) (suspendIntentRecoveryResult, error) {
	result := suspendIntentRecoveryResult{Cursor: cursor}
	if _, finite := ctx.Deadline(); !finite || ctx.Err() != nil || now.IsZero() || stopAndVerify == nil {
		return result, errors.New("browser suspend recovery requires a finite budget, clock and teardown adapter")
	}

	s.mu.Lock()
	if err := s.private(); err != nil {
		s.mu.Unlock()
		return result, errors.New("browser suspend recovery registry unavailable")
	}
	records, err := s.snapshot(ctx)
	if err == nil {
		// A visible intent can follow an interrupted directory flush. Its
		// visibility alone is not a receipt that admits the teardown effect.
		err = s.syncDir()
	}
	if err == nil {
		err = ctx.Err()
	}
	s.mu.Unlock()
	if err != nil {
		return result, errors.New("browser suspend recovery scan failed")
	}
	result.Scanned = len(records)
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	start := sort.Search(len(records), func(i int) bool { return records[i].ID > cursor })
	for i := 0; i < len(records); i++ {
		if result.Attempted == maxSuspendRecoveryTransitions {
			break
		}
		if ctx.Err() != nil {
			return result, errors.New("browser suspend recovery budget expired")
		}
		rec := records[(start+i)%len(records)]
		result.Cursor = rec.ID
		if rec.State != "suspending" {
			continue // Never reopen, unlock or infer a stop for another state.
		}
		result.Attempted++
		if _, err := s.completeSuspending(ctx, rec, now, stopAndVerify); err != nil {
			if _, err := s.failSuspending(ctx, rec, now, stopAndVerify); err != nil {
				result.Failed++
			} else {
				result.Terminated++
			}
		} else {
			result.Completed++
		}
	}
	if result.Failed > 0 {
		return result, errors.New("browser suspend recovery has incomplete transitions")
	}
	return result, nil
}
