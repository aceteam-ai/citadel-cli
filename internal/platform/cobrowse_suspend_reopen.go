package platform

// Unwired persistence for an already-planned reopen. This does not authorize
// a request, consume an unlock, decrypt a capture, reserve a browser slot or
// launch anything. The manager must perform those checks independently and
// exclude legacy record writers for the entire lifecycle transaction.

import (
	"context"
	"errors"
	"math"
	"reflect"
	"time"
)

// stageResuming accepts the exact value produced by planReopenRecord. Only
// the transition's commit time is supplied here. A visible-but-unflushed retry
// reconciles that same intent; it never increments the live generation twice
// or changes the capture generation, TTL or payload.
func (s *suspendIntentStore) stageResuming(ctx context.Context, expected, planned sessionRecord,
	now time.Time, fence func() error) (sessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	refuse := func() (sessionRecord, error) {
		return sessionRecord{}, errors.New("could not persist browser reopen intent")
	}
	check := func() bool {
		return ctx.Err() == nil && fence != nil && fence() == nil && s.private() == nil
	}
	if _, finite := ctx.Deadline(); !finite || now.IsZero() || !check() ||
		expected.State != RecordSuspended || expected.Generation == math.MaxUint64 {
		return refuse()
	}
	expiry, err := suspendExpiry(expected)
	if err != nil || !now.Before(expiry) {
		return refuse()
	}
	want := expected
	want.State, want.Generation = "resuming", expected.Generation+1
	// The pure planner preserves UpdatedAt; only this durable transition
	// owns changing it. Do not accept unrelated changes hidden in the plan.
	if !reflect.DeepEqual(want, planned) {
		return refuse()
	}
	want.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	if _, err := suspendExpiry(want); err != nil {
		return refuse()
	}
	current, err := s.read(expected.ID)
	if err != nil {
		return refuse()
	}
	if current.State == "resuming" {
		// A prior atomic rename can be visible despite a failed directory
		// flush. Preserve its timestamp and reconcile durability only.
		want.UpdatedAt = current.UpdatedAt
		if _, err := suspendExpiry(current); err != nil {
			return refuse()
		}
		if !reflect.DeepEqual(current, want) || s.payloads.checkPayload(current) != nil ||
			s.syncDir() != nil || !check() {
			return refuse()
		}
		return current, nil
	}
	if !reflect.DeepEqual(current, expected) || s.payloads.checkPayload(expected) != nil || !check() {
		return refuse()
	}
	if _, err := s.snapshot(ctx); err != nil || !check() {
		return refuse()
	}
	if err := s.replaceRecord(expected, want, check); err != nil {
		return refuse()
	}
	return want, nil
}
