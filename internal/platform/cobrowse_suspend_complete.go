package platform

// Unwired post-capture lifecycle completion. The manager supplies an
// idempotent, identity-pinned stop-and-verify operation and retains lifecycle
// exclusion. No browser runner, job caller or credential operation is installed
// here. A callback error must never be interpreted as proof of teardown.

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"runtime"
	"time"
)

// Value-only process facts. The teardown adapter receives no profile material,
// page data, account identity or mutable pointers from the lifecycle record.
type suspendStopIdentity struct {
	SessionID, BrowserID, Runner, ContainerID, ContainerStart string
	BrowserScope, XvfbScope                                   string
	Generation, BrowserStart, XvfbStart                       uint64
	BrowserPID, XvfbPID, DebugPort, ControlPort               int
	ScopeUser                                                 bool
}

func suspendIdentity(rec sessionRecord) suspendStopIdentity {
	return suspendStopIdentity{SessionID: rec.ID, BrowserID: rec.BrowserID, Runner: rec.Runner,
		ContainerID: rec.ContainerID, ContainerStart: rec.ContainerStart,
		BrowserScope: rec.BrowserScope, XvfbScope: rec.XvfbScope, ScopeUser: rec.ScopeUser,
		Generation: rec.Generation, BrowserStart: rec.BrowserStart, XvfbStart: rec.XvfbStart,
		BrowserPID: rec.BrowserPID, XvfbPID: rec.XvfbPID, DebugPort: rec.DebugPort, ControlPort: rec.ControlPort}
}

// checkPayload checks only the bounded, private file envelope. It never
// decrypts, interprets or returns page contents. The capture stage is
// responsible for preparing valid data before publishing the intent.
func (s *suspendManifestStore) checkPayload(rec sessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	refuse := func() error { return errors.New("complete browser capture is unavailable") }
	if s.private() != nil {
		return refuse()
	}
	name, err := suspendManifestName(rec)
	if err != nil {
		return refuse()
	}
	info, err := s.root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() != rec.Suspension.ManifestBytes ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return refuse()
	}
	f, err := s.root.Open(name)
	if err != nil {
		return refuse()
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return refuse()
	}
	n, err := io.Copy(io.Discard, io.LimitReader(f, maxSuspendManifestBytes+4096+1))
	if err != nil || n != rec.Suspension.ManifestBytes {
		return refuse()
	}
	return nil
}

// completeSuspending does not stop a real process itself. stopAndVerify must
// operate only on the exact identities in intent and return nil only once the
// browser, display and debug-port resources have been released. It must honor
// ctx, tolerate an already-stopped incarnation, and not re-enter this store.
// The caller must exclude both legacy record writers and concurrent lifecycle
// operations throughout this call. The store's mutex alone is insufficient.
func (s *suspendIntentStore) completeSuspending(ctx context.Context, intent sessionRecord, now time.Time,
	stopAndVerify func(context.Context, suspendStopIdentity) error) (sessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	refuse := func() (sessionRecord, error) {
		return sessionRecord{}, errors.New("could not complete suspended browser teardown")
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
	final.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	current, err := s.read(intent.ID)
	if err != nil {
		return refuse()
	}
	if current.State == RecordSuspended {
		// A prior rename may be visible even though its directory flush
		// failed. Reconcile the exact completed state without a second stop
		// or refreshed timestamp. A newer/different record must not match.
		committed, parseErr := time.Parse(time.RFC3339Nano, current.UpdatedAt)
		if parseErr != nil || committed.IsZero() || committed.Before(started) {
			return refuse()
		}
		final.UpdatedAt = current.UpdatedAt
		if !reflect.DeepEqual(current, final) || s.payloads.checkPayload(current) != nil || s.syncDir() != nil || !check() {
			return refuse()
		}
		return current, nil
	}
	if !reflect.DeepEqual(current, intent) {
		return refuse()
	}
	identity := suspendIdentity(intent)
	if identity.BrowserID == "" || identity.DebugPort < 1 || identity.DebugPort > 65535 ||
		(identity.BrowserPID <= 0 && identity.ContainerID == "") {
		return refuse()
	}
	// Do not cross the teardown boundary if the capture is absent or if
	// the subsequent bounded registry replacement has no reserved space.
	if s.payloads.checkPayload(intent) != nil || !check() {
		return refuse()
	}
	if _, err := s.snapshot(ctx); err != nil || !check() {
		return refuse()
	}
	current, err = s.read(intent.ID)
	if err != nil || !reflect.DeepEqual(current, intent) || !check() {
		return refuse()
	}
	if err := stopAndVerify(ctx, identity); err != nil || !check() {
		return refuse()
	}
	// Teardown is an awaited effect. Do not publish restorable suspension if
	// its capture disappeared or became incomplete during that boundary.
	if s.payloads.checkPayload(intent) != nil || !check() {
		return refuse()
	}
	if err := s.replaceRecord(intent, final, check); err != nil {
		return refuse()
	}
	return final, nil
}
