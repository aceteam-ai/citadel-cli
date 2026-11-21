package platform

// Unwired TTL effects for #1821. This only manages the tab payload and the
// existing lifecycle record. It never removes a profile or touches a browser.
// The manager must hold lifecycle exclusion before calling either transition;
// the store mutex is not a substitute for exclusion from legacy record writers.

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sort"
	"time"
)

const maxSuspendTTLTransitions = 16

// Metadata-only progress. The caller must retain Cursor even on a partial
// failure, so one unrecoverable filesystem entry cannot starve its siblings.
type suspendTTLSweepResult struct {
	Scanned, Attempted, Expired, Pruned, Failed int
	Cursor                                      string
}

// sweepTTL performs one bounded, level-triggered pass. It installs no timer;
// the eventual manager scheduler supplies the clock, finite context, cursor
// and lifecycle exclusion. Every transition re-reads its snapshot before use.
func (s *suspendIntentStore) sweepTTL(ctx context.Context, now time.Time, cursor string) (suspendTTLSweepResult, error) {
	result := suspendTTLSweepResult{Cursor: cursor}
	if _, finite := ctx.Deadline(); !finite || ctx.Err() != nil || now.IsZero() {
		return result, errors.New("browser TTL sweep requires a finite budget and clock")
	}
	s.mu.Lock()
	if err := s.private(); err != nil {
		s.mu.Unlock()
		return result, errors.New("browser TTL registry unavailable")
	}
	records, err := s.snapshot(ctx)
	if err == nil {
		// Reconcile visible metadata from an earlier interrupted pass in
		// one flush, including tombstones not yet due for deletion. Without
		// this, an uncertain expired-record rename could be skipped for days.
		err = s.syncDir()
	}
	if err == nil {
		err = ctx.Err()
	}
	s.mu.Unlock()
	if err != nil {
		return result, errors.New("browser TTL registry scan failed")
	}
	result.Scanned = len(records)
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	start := sort.Search(len(records), func(i int) bool { return records[i].ID > cursor })
	for i := 0; i < len(records); i++ {
		if result.Attempted == maxSuspendTTLTransitions {
			break
		}
		if ctx.Err() != nil {
			return result, errors.New("browser TTL sweep budget expired")
		}
		r := records[(start+i)%len(records)]
		result.Cursor = r.ID
		switch r.State {
		case RecordSuspended:
			_, due, planErr := planSuspendExpiry(r, now)
			if planErr != nil {
				return result, errors.New("browser TTL record could not be planned")
			}
			if !due {
				continue
			}
			result.Attempted++
			_, changed, err := s.expire(ctx, r, now)
			if err != nil {
				result.Failed++
			} else if changed {
				result.Expired++
			}
		case RecordExpired:
			if !suspendTombstoneDue(r, now) {
				continue
			}
			result.Attempted++
			removed, err := s.pruneTombstone(ctx, r, now)
			if err != nil {
				result.Failed++
			} else if removed {
				result.Pruned++
			}
		}
	}
	if result.Failed > 0 {
		return result, errors.New("browser TTL sweep has incomplete transitions")
	}
	return result, nil
}

func (s *suspendIntentStore) expire(ctx context.Context, expected sessionRecord, now time.Time) (sessionRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func() (sessionRecord, bool, error) {
		return sessionRecord{}, false, errors.New("could not expire suspended browser tabs")
	}
	check := func() bool { return ctx.Err() == nil && s.private() == nil }
	if _, finite := ctx.Deadline(); !finite || now.IsZero() || !check() {
		return fail()
	}
	current, err := s.read(expected.ID)
	if err != nil || !reflect.DeepEqual(current, expected) {
		return fail()
	}
	next, due, err := planSuspendExpiry(current, now)
	if err != nil {
		return fail()
	}
	if !due {
		// A previous directory flush may have failed after publishing the
		// tombstone. Reconcile its durability on the retry, not just its
		// currently visible state. Never refresh the original expiry.
		if current.State == RecordExpired && current.EndedReason == "suspend_expired" &&
			(s.syncDir() != nil || !check()) {
			return fail()
		}
		return current, false, nil
	}
	// Bound physical registry usage, including the replacement temporary
	// record, before deleting anything. Never silently skip malformed peers.
	if _, err := s.snapshot(ctx); err != nil || !check() {
		return fail()
	}
	current, err = s.read(expected.ID)
	if err != nil || !reflect.DeepEqual(current, expected) || !check() {
		return fail()
	}
	// The persistent profile is not under the payload root and is never
	// opened, unlocked, sealed or removed by expiry. Missing tab files are
	// idempotent after a crash between deletion and the registry commit.
	if err := s.payloads.remove(current); err != nil || !check() {
		return fail()
	}
	next.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	if err := s.replaceRecord(current, next, check); err != nil {
		return fail()
	}
	return next, true, nil
}

func (s *suspendIntentStore) pruneTombstone(ctx context.Context, expected sessionRecord, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func() (bool, error) { return false, errors.New("could not prune expired browser tombstone") }
	check := func() bool { return ctx.Err() == nil && s.private() == nil }
	if _, finite := ctx.Deadline(); !finite || now.IsZero() || !check() {
		return fail()
	}
	if !sessionIDPattern.MatchString(expected.ID) || len(expected.ID) > 64 || expected.Version != sessionRecordVersion {
		return fail()
	}
	current, err := s.read(expected.ID)
	if err != nil {
		// An interrupted prune may already have removed the record. Flush
		// its absence before acknowledging an idempotent retry.
		if _, statErr := s.root.Lstat(expected.ID + ".json"); errors.Is(statErr, os.ErrNotExist) &&
			suspendTombstoneDue(expected, now) && s.syncDir() == nil && check() {
			return true, nil
		}
		return fail()
	}
	if !reflect.DeepEqual(current, expected) {
		return fail()
	}
	if !suspendTombstoneDue(current, now) {
		return false, nil
	}
	// Only the one verified lifecycle record is removed. No payload/profile
	// cleanup is inferred from a tombstone, and no directory walk is needed.
	if !check() || s.removeRecord(current.ID+".json") != nil || s.syncDir() != nil || !check() {
		return fail()
	}
	return true, nil
}
