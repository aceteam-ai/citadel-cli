package platform

// Unwired recovery of a durable suspend intent whose capture is absent. The
// eventual manager must exclude legacy writers and supply the trusted,
// idempotent exact-incarnation teardown adapter. No process adapter or manager
// caller is installed here.

import (
	"context"
	"errors"
	"os"
	"reflect"
	"time"
)

// confirmAbsent never interprets an unreadable or malformed capture as absent.
// Flush and recheck the private directory before relying on a missing entry.
// Nothing is removed, opened as a profile, decrypted or exposed to the caller.
func (s *suspendManifestStore) confirmAbsent(rec sessionRecord) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func() (bool, error) { return false, errors.New("browser capture absence is uncertain") }
	if s.private() != nil {
		return fail()
	}
	name, err := suspendManifestName(rec)
	if err != nil {
		return fail()
	}
	if _, err := s.root.Lstat(name); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail()
	}
	if s.syncDir() != nil || s.private() != nil {
		return fail()
	}
	if _, err := s.root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return fail()
	}
	return true, nil
}

// failSuspending records an ended session, not a successful suspend. A missing
// capture alone is never evidence that the browser/display stopped. Like the
// completion helper, this requires a finite budget and lifecycle exclusion;
// a callback refusal or uncertain commit remains retryable and opaque.
func (s *suspendIntentStore) failSuspending(ctx context.Context, intent sessionRecord, now time.Time,
	stopAndVerify func(context.Context, suspendStopIdentity) error) (sessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	refuse := func() (sessionRecord, error) {
		return sessionRecord{}, errors.New("could not reconcile failed browser suspension")
	}
	check := func() bool { return ctx.Err() == nil && s.private() == nil }
	if _, finite := ctx.Deadline(); !finite || now.IsZero() || stopAndVerify == nil || !check() || intent.State != "suspending" {
		return refuse()
	}
	if _, err := suspendExpiry(intent); err != nil {
		return refuse()
	}
	started, err := time.Parse(time.RFC3339Nano, intent.Suspension.SuspendedAt)
	if err != nil || now.Before(started) {
		return refuse()
	}
	live := intent
	live.State = RecordLive
	final, err := planSuspendedRecord(live, intent.OwnerUserID, intent.ProfileName, intent.Suspension.TabCount,
		intent.Suspension.ManifestBytes, nil, started)
	if err != nil {
		return refuse()
	}
	final.State, final.EndedReason = RecordExpired, "suspend_failed"
	final.EndedAt, final.UpdatedAt = now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano)
	final.Suspension = nil // No restorable capture; no reopen is admitted.
	current, err := s.read(intent.ID)
	if err != nil {
		return refuse()
	}
	if current.State == RecordExpired {
		ended, parseErr := time.Parse(time.RFC3339Nano, current.EndedAt)
		if parseErr != nil || ended.IsZero() || ended.Before(started) || current.UpdatedAt != current.EndedAt {
			return refuse()
		}
		final.EndedAt, final.UpdatedAt = current.EndedAt, current.UpdatedAt
		if !reflect.DeepEqual(current, final) {
			return refuse()
		}
		missing, err := s.payloads.confirmAbsent(intent)
		if err != nil || !missing || s.syncDir() != nil || !check() {
			return refuse()
		}
		return current, nil // Reconcile a visible commit without another stop.
	}
	if !reflect.DeepEqual(current, intent) {
		return refuse()
	}
	identity := suspendIdentity(intent)
	if identity.BrowserID == "" || identity.DebugPort < 1 || identity.DebugPort > 65535 ||
		(identity.BrowserPID <= 0 && identity.ContainerID == "") {
		return refuse()
	}
	missing, err := s.payloads.confirmAbsent(intent)
	if err != nil || !missing || !check() {
		return refuse()
	}
	if _, err := s.snapshot(ctx); err != nil || s.syncDir() != nil || !check() {
		return refuse()
	}
	current, err = s.read(intent.ID)
	if err != nil || !reflect.DeepEqual(current, intent) || !check() {
		return refuse()
	}
	if err := stopAndVerify(ctx, identity); err != nil || !check() {
		return refuse()
	}
	missing, err = s.payloads.confirmAbsent(intent)
	if err != nil || !missing || !check() {
		return refuse()
	}
	if s.replaceRecord(intent, final, check) != nil {
		return refuse()
	}
	return final, nil
}
