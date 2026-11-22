package platform

// Unwired interrupted-reopen rollback. No process, profile or public caller is
// installed. The manager must exclude legacy writers and supply positive,
// idempotent proof that ALL provisional resources of this exact attempt are
// stopped. Unknown resource provenance must refuse, never imply absence.

import (
	"context"
	"errors"
	"reflect"
	"time"
)

type reopenAttemptIdentity struct {
	SessionID  string
	Generation uint64
}

// rollbackResuming retains the advanced live generation: an attempted
// incarnation is never reused. Capture generation and expiry are immutable.
// The rollback receipt is private registry metadata, not a new wire state.
func (s *suspendIntentStore) rollbackResuming(ctx context.Context, intent sessionRecord, now time.Time,
	verifyAttemptStopped func(context.Context, reopenAttemptIdentity) error) (sessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	refuse := func() (sessionRecord, error) {
		return sessionRecord{}, errors.New("could not reconcile interrupted browser reopen")
	}
	check := func() bool { return ctx.Err() == nil && s.private() == nil }
	if _, finite := ctx.Deadline(); !finite || now.IsZero() || verifyAttemptStopped == nil || !check() || intent.State != "resuming" {
		return refuse()
	}
	if _, err := suspendExpiry(intent); err != nil {
		return refuse()
	}
	started, err := time.Parse(time.RFC3339Nano, intent.UpdatedAt)
	if err != nil || started.IsZero() || now.Before(started) {
		return refuse()
	}
	captured, err := time.Parse(time.RFC3339Nano, intent.Suspension.SuspendedAt)
	if err != nil || started.Before(captured) {
		return refuse()
	}
	live := intent
	live.State = RecordLive
	final, err := planSuspendedRecord(live, intent.OwnerUserID, intent.ProfileName, intent.Suspension.TabCount,
		intent.Suspension.ManifestBytes, nil, captured)
	if err != nil {
		return refuse()
	}
	meta := *intent.Suspension
	meta.ReopenRollback = &suspendReopenRollback{Generation: intent.Generation, At: now.UTC().Format(time.RFC3339Nano)}
	final.Suspension, final.UpdatedAt = &meta, meta.ReopenRollback.At
	current, err := s.read(intent.ID)
	if err != nil {
		return refuse()
	}
	if current.State == RecordSuspended {
		// Reconcile an uncertain visible commit, preserving its original
		// receipt, without stopping again or refreshing retention.
		if _, err := suspendExpiry(current); err != nil || current.Suspension.ReopenRollback == nil {
			return refuse()
		}
		at, err := time.Parse(time.RFC3339Nano, current.Suspension.ReopenRollback.At)
		if err != nil || at.Before(started) {
			return refuse()
		}
		meta.ReopenRollback = &suspendReopenRollback{Generation: intent.Generation, At: current.Suspension.ReopenRollback.At}
		final.UpdatedAt = current.UpdatedAt
		if !reflect.DeepEqual(current, final) || s.payloads.checkPayload(current) != nil || s.syncDir() != nil || !check() {
			return refuse()
		}
		return current, nil
	}
	if !reflect.DeepEqual(current, intent) || s.payloads.checkPayload(intent) != nil || !check() {
		return refuse()
	}
	if _, err := s.snapshot(ctx); err != nil || s.syncDir() != nil || !check() {
		return refuse()
	}
	current, err = s.read(intent.ID)
	if err != nil || !reflect.DeepEqual(current, intent) || !check() {
		return refuse()
	}
	identity := reopenAttemptIdentity{SessionID: intent.ID, Generation: intent.Generation}
	if err := verifyAttemptStopped(ctx, identity); err != nil || !check() {
		return refuse()
	}
	if s.payloads.checkPayload(intent) != nil || !check() {
		return refuse()
	}
	if _, err := suspendExpiry(final); err != nil || s.replaceRecord(intent, final, check) != nil {
		return refuse()
	}
	return final, nil
}
