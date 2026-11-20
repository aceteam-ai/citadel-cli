package platform

// Unwired durable reopen completion. No runner, job, credential operation or
// timer is installed. The manager must hold lifecycle exclusion and supply
// positive proof of the exact live incarnation AND its complete restored tabs.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"
)

// completeResuming publishes a runner's proposed live record only after the
// manager verifies its processes, pages, selection and scroll restoration.
// verifyRestored must honor ctx, have no launch effects and never re-enter this
// store. fence must reject a changed manager/session incarnation. Neither is
// authorization. The caller must already have established request authority.
//
// The old capture is NOT deleted here: an uncertain rename may still need it.
// Once the live record is durable, the bounded orphan collector can reclaim it.
func (s *suspendIntentStore) completeResuming(ctx context.Context, intent, proposed sessionRecord, now time.Time,
	fence func() error, verifyRestored func(context.Context, suspendStopIdentity) error) (sessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	refuse := func() (sessionRecord, error) {
		return sessionRecord{}, errors.New("could not complete browser reopen")
	}
	if ctx == nil {
		return refuse()
	}
	check := func() bool { return ctx.Err() == nil && fence != nil && fence() == nil && s.private() == nil }
	if _, finite := ctx.Deadline(); !finite || now.IsZero() || verifyRestored == nil || !check() || intent.State != "resuming" {
		return refuse()
	}
	expiry, err := suspendExpiry(intent)
	started, timeErr := time.Parse(time.RFC3339Nano, intent.UpdatedAt)
	launched, launchErr := time.Parse(time.RFC3339Nano, proposed.StartedAt)
	captured := time.Time{}
	if err == nil {
		captured, err = time.Parse(time.RFC3339Nano, intent.Suspension.SuspendedAt)
	}
	if err != nil || timeErr != nil || launchErr != nil || started.IsZero() || started.Before(captured) || launched.Before(started) ||
		now.Before(launched) || !now.Before(expiry) {
		return refuse()
	}
	// Compare the logical session using the planner's resource-free projection.
	// Fresh runtime facts are checked below and by the restoration verifier;
	// terminal/suspension fields are explicitly refused rather than projected.
	wantLogical := intent
	wantLogical.State, wantLogical.Suspension = RecordLive, nil
	wantLogical.StartedAt = proposed.StartedAt
	logical, err := planSuspendedRecord(proposed, intent.OwnerUserID, intent.ProfileName,
		intent.Suspension.TabCount, intent.Suspension.ManifestBytes, nil, started)
	want, wantErr := planSuspendedRecord(wantLogical, intent.OwnerUserID, intent.ProfileName,
		intent.Suspension.TabCount, intent.Suspension.ManifestBytes, nil, started)
	if err != nil || wantErr != nil || !reflect.DeepEqual(logical, want) || proposed.State != RecordLive ||
		proposed.Suspension != nil || proposed.UpdatedAt != intent.UpdatedAt || proposed.PendingEnd != nil ||
		proposed.EndedReason != "" || proposed.EndedDetail != "" || proposed.EndedAt != "" {
		return refuse()
	}
	// A resuming intent carries the capture, not an old live incarnation.
	// Refuse corrupt/stale process or terminal facts instead of clearing them
	// as an accidental side effect of publishing the new runner's resources.
	canonicalIntent := want
	canonicalIntent.State, canonicalIntent.Suspension = "resuming", intent.Suspension
	canonicalIntent.StartedAt, canonicalIntent.UpdatedAt = intent.StartedAt, intent.UpdatedAt
	if !reflect.DeepEqual(canonicalIntent, intent) {
		return refuse()
	}
	identity := suspendIdentity(proposed)
	if identity.BrowserID == "" || identity.DebugPort < 1 || identity.DebugPort > 65535 || proposed.Profile == "" ||
		(identity.Runner != "" && identity.Runner != "container") ||
		(identity.Runner == "" && (identity.BrowserPID <= 0 || identity.ContainerID != "" || identity.ControlPort != 0)) ||
		(identity.Runner == "container" && (identity.ContainerID == "" || identity.ContainerStart == "" ||
			identity.ControlPort < 1 || identity.ControlPort > 65535 || identity.BrowserPID != 0)) {
		return refuse()
	}
	// Detach the only mutable pointer retained from the runner's proposal.
	if proposed.OwnerUID != nil {
		uid := *proposed.OwnerUID
		proposed.OwnerUID = &uid
	}
	if data, err := json.Marshal(proposed); err != nil || len(data) > maxSuspendRegistryRecordBytes {
		return refuse()
	}
	proposed.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	current, err := s.read(intent.ID)
	if err != nil {
		return refuse()
	}
	replay := current.State == RecordLive
	if replay {
		committed, parseErr := time.Parse(time.RFC3339Nano, current.UpdatedAt)
		if parseErr != nil || committed.Before(launched) || now.Before(committed) {
			return refuse()
		}
		proposed.UpdatedAt = current.UpdatedAt
		if !reflect.DeepEqual(current, proposed) {
			return refuse()
		}
	} else if !reflect.DeepEqual(current, intent) || s.payloads.checkPayload(intent) != nil {
		return refuse()
	}
	if _, err := s.snapshot(ctx); err != nil || !check() {
		return refuse()
	}
	if err := verifyRestored(ctx, identity); err != nil || !check() {
		return refuse()
	}
	// Re-read after the awaited proof, including on a visible-commit retry.
	again, err := s.read(intent.ID)
	if err != nil || !reflect.DeepEqual(again, current) || !check() {
		return refuse()
	}
	if replay {
		if s.syncDir() != nil || !check() {
			return refuse()
		}
	} else if s.payloads.checkPayload(intent) != nil || s.replaceRecord(intent, proposed, check) != nil {
		return refuse()
	}
	return proposed, nil
}
