package platform

import (
	"context"
	"errors"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func suspendRollbackFixture(t *testing.T) (*suspendIntentStore, string, sessionRecord, sessionRecord, time.Time) {
	t.Helper()
	s, dir, suspended, planned, now := suspendReopenFixture(t)
	intent, err := s.stageResuming(suspendPagesContext(t), suspended, planned, now, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return s, dir, suspended, intent, now.Add(time.Minute)
}

func TestSuspendRollbackInterruptedReopenThenReopen(t *testing.T) {
	s, dir, original, intent, now := suspendRollbackFixture(t)
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	s, err := openSuspendIntentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	current, err := s.read(original.ID)
	if err != nil || !reflect.DeepEqual(current, intent) {
		t.Fatal("interrupted reopen intent did not survive store restart")
	}
	var order []string
	flush, syncFile := s.syncDir, s.syncFile
	s.syncDir = func() error { order = append(order, "flush"); return flush() }
	s.syncFile = func(f *os.File) error { order = append(order, "file"); return syncFile(f) }
	stop := func(ctx context.Context, identity reopenAttemptIdentity) error {
		if _, finite := ctx.Deadline(); !finite || identity != (reopenAttemptIdentity{SessionID: original.ID, Generation: intent.Generation}) {
			t.Error("rollback lost exact attempt or finite budget")
		}
		live, err := s.read(original.ID)
		if err != nil || !reflect.DeepEqual(live, intent) {
			t.Error("rollback committed before provisional resources were verified stopped")
		}
		order = append(order, "verified-stop")
		return nil
	}
	rolled, err := s.rollbackResuming(suspendPagesContext(t), intent, now, stop)
	if err != nil || rolled.ID != original.ID || rolled.Generation != intent.Generation || rolled.State != RecordSuspended ||
		!reflect.DeepEqual(order, []string{"flush", "verified-stop", "file", "flush"}) {
		t.Fatal("rollback reused generation or acknowledged before durable verification")
	}
	if rolled.Suspension.ReopenRollback == nil || rolled.Suspension.ReopenRollback.Generation != intent.Generation || rolled.Suspension.ReopenRollback.At != now.Format(time.RFC3339Nano) {
		t.Fatal("rollback lacks an exact recorded receipt")
	}
	meta := *rolled.Suspension
	meta.ReopenRollback = nil
	if !reflect.DeepEqual(meta, *original.Suspension) || intent.Suspension.ReopenRollback != nil {
		t.Fatal("rollback mutated original capture, TTL or caller intent")
	}
	manifest, err := s.payloads.read(rolled, nil)
	if err != nil || manifest.Generation != original.Generation {
		t.Fatal("advanced live generation lost immutable capture lookup")
	}
	if records, err := s.snapshot(suspendPagesContext(t)); err != nil || len(records) != 1 {
		t.Fatal("recorded rollback is not valid bounded registry state")
	}
	for attempt := 0; attempt < 2; attempt++ {
		nextTime := now.Add(time.Duration(attempt*2+1) * time.Minute)
		planned, err := planReopenRecord(rolled, rolled.OwnerUserID, false, nextTime)
		if err != nil || planned.Generation != rolled.Generation+1 {
			t.Fatal("next reopen did not advance beyond the failed attempt")
		}
		next, err := s.stageResuming(suspendPagesContext(t), rolled, planned, nextTime, func() error { return nil })
		if err != nil || next.Generation != intent.Generation+uint64(attempt)+1 || next.Suspension.CaptureGeneration != original.Generation || next.Suspension.ExpiresAt != original.Suspension.ExpiresAt {
			t.Fatal("repeat reopen reused a generation or refreshed capture retention")
		}
		if _, err := s.payloads.read(next, nil); err != nil {
			t.Fatal("repeat reopen could not read the original payload")
		}
		if _, err := s.rollbackResuming(suspendPagesContext(t), intent, nextTime.Add(time.Second), func(context.Context, reopenAttemptIdentity) error {
			t.Error("stale rollback reached cleanup of a newer attempt")
			return nil
		}); err == nil {
			t.Fatal("stale rollback replaced the next reopen incarnation")
		}
		rolled, err = s.rollbackResuming(suspendPagesContext(t), next, nextTime.Add(time.Minute), func(_ context.Context, id reopenAttemptIdentity) error {
			if id.Generation != next.Generation {
				t.Error("retry cleanup used a previous incarnation")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// TTL is the original capture deadline even after multiple failed attempts.
	expiry, _ := suspendExpiry(rolled)
	if _, err := planReopenRecord(rolled, rolled.OwnerUserID, false, expiry); err == nil {
		t.Fatal("rollback refreshed expired capture eligibility")
	}
	expired, due, err := s.expire(suspendPagesContext(t), rolled, expiry)
	if err != nil || !due || expired.Generation != rolled.Generation || expired.EndedAt != original.Suspension.ExpiresAt {
		t.Fatal("rollback changed expiry time or tombstone generation")
	}
}

func TestSuspendRollbackGenerationValidationStaysStrict(t *testing.T) {
	s, _, original, intent, now := suspendRollbackFixture(t)
	rolled, err := s.rollbackResuming(suspendPagesContext(t), intent, now, func(context.Context, reopenAttemptIdentity) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"unmarked-suspended", "unmarked-resuming", "wrong-receipt-generation", "unrecorded-later-generation", "receipt-at-capture-generation", "missing-time", "before-capture", "different-updated", "marked-suspending", "marked-live", "resuming-skips-generation", "overflow"} {
		t.Run(kind, func(t *testing.T) {
			rec := rolled
			meta, receipt := *rolled.Suspension, *rolled.Suspension.ReopenRollback
			meta.ReopenRollback, rec.Suspension = &receipt, &meta
			switch kind {
			case "unmarked-suspended":
				meta.ReopenRollback = nil
			case "unmarked-resuming":
				rec.State, rec.Generation, meta.ReopenRollback = "resuming", rolled.Generation+1, nil
			case "wrong-receipt-generation":
				receipt.Generation++
			case "unrecorded-later-generation":
				rec.Generation++
			case "receipt-at-capture-generation":
				receipt.Generation, rec.Generation = original.Generation, original.Generation
			case "missing-time":
				receipt.At = ""
			case "before-capture":
				receipt.At = suspendTestNow.Add(-time.Nanosecond).Format(time.RFC3339Nano)
				rec.UpdatedAt = receipt.At
			case "different-updated":
				rec.UpdatedAt = now.Add(time.Second).Format(time.RFC3339Nano)
			case "marked-suspending":
				rec.State = "suspending"
			case "marked-live":
				rec.State = RecordLive
			case "resuming-skips-generation":
				rec.State, rec.Generation = "resuming", rolled.Generation+2
			case "overflow":
				receipt.Generation, rec.State, rec.Generation = math.MaxUint64, "resuming", 1
			}
			if _, err := suspendExpiry(rec); err == nil {
				t.Fatal("unrecorded, stale or malformed rollback relaxed generation validation")
			}
		})
	}
	if _, err := suspendExpiry(original); err != nil {
		t.Fatal("ordinary suspended record stopped validating")
	}
	if _, err := suspendExpiry(intent); err != nil {
		t.Fatal("ordinary resuming record stopped validating")
	}
}

func TestSuspendRollbackRefusalsKeepIntent(t *testing.T) {
	for _, kind := range []string{"deadline", "cancelled", "zero-clock", "before-intent", "wrong-state", "stale-generation", "stale-record", "nil-proof", "missing-payload", "truncated-payload", "bad-registry", "flush-refusal"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _, intent, now := suspendRollbackFixture(t)
			ctx, expected := suspendPagesContext(t), intent
			calls := 0
			stop := func(context.Context, reopenAttemptIdentity) error { calls++; return nil }
			switch kind {
			case "deadline":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "zero-clock":
				now = time.Time{}
			case "before-intent":
				now = now.Add(-2 * time.Minute)
			case "wrong-state":
				expected.State = RecordSuspended
			case "stale-generation":
				expected.Generation++
			case "stale-record":
				expected.Handoff = !expected.Handoff
			case "nil-proof":
				stop = nil
			case "missing-payload":
				name, _ := suspendManifestName(intent)
				if err := s.payloads.root.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "truncated-payload":
				name, _ := suspendManifestName(intent)
				f, err := s.payloads.root.OpenFile(name, os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
			case "bad-registry":
				if err := saveRecord(s.root.Name(), sessionRecord{Version: sessionRecordVersion, ID: "cb-bad", State: "unknown"}); err != nil {
					t.Fatal(err)
				}
			case "flush-refusal":
				s.syncDir = func() error { return errors.New("private-rollback-sentinel") }
			}
			if out, err := s.rollbackResuming(ctx, expected, now, stop); err == nil || out.ID != "" || calls != 0 || strings.Contains(err.Error(), "private-rollback-sentinel") {
				t.Fatal("invalid rollback crossed the cleanup boundary or exposed details")
			}
			current, err := s.read(intent.ID)
			if err != nil || !reflect.DeepEqual(current, intent) {
				t.Fatal("refused rollback changed its intent")
			}
		})
	}
}

func TestSuspendRollbackFailureReplay(t *testing.T) {
	for _, phase := range []string{"stop", "file-sync", "rename", "directory-sync", "cancel-after-stop", "capture-after-stop"} {
		t.Run(phase, func(t *testing.T) {
			s, _, _, intent, now := suspendRollbackFixture(t)
			ctx, cancel := context.WithCancel(suspendPagesContext(t))
			defer cancel()
			flush, syncFile, rename := s.syncDir, s.syncFile, s.rename
			calls, flushes := 0, 0
			stop := func(context.Context, reopenAttemptIdentity) error { calls++; return nil }
			refusal := errors.New("private-rollback-sentinel")
			switch phase {
			case "stop":
				stop = func(context.Context, reopenAttemptIdentity) error { calls++; return refusal }
			case "file-sync":
				s.syncFile = func(*os.File) error { return refusal }
			case "rename":
				s.rename = func(string, string) error { return refusal }
			case "directory-sync":
				s.syncDir = func() error {
					flushes++
					if flushes > 1 {
						return refusal
					}
					return flush()
				}
			case "cancel-after-stop":
				stop = func(context.Context, reopenAttemptIdentity) error { calls++; cancel(); return nil }
			case "capture-after-stop":
				stop = func(context.Context, reopenAttemptIdentity) error {
					calls++
					name, _ := suspendManifestName(intent)
					return s.payloads.root.Remove(name)
				}
			}
			if out, err := s.rollbackResuming(ctx, intent, now, stop); err == nil || out.ID != "" || calls != 1 || strings.Contains(err.Error(), "private-rollback-sentinel") {
				t.Fatal("failed rollback acknowledged success or skipped verification")
			}
			current, err := s.read(intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if phase != "directory-sync" && !reflect.DeepEqual(current, intent) {
				t.Fatal("refused rollback changed its recoverable state")
			}
			if phase == "directory-sync" {
				before := calls
				if _, err := s.rollbackResuming(suspendPagesContext(t), intent, now.Add(time.Second), stop); err == nil || calls != before {
					t.Fatal("visible rollback bypassed directory durability or repeated cleanup")
				}
			}
			if phase == "capture-after-stop" {
				return
			}
			s.syncDir, s.syncFile, s.rename = flush, syncFile, rename
			before := calls
			out, err := s.rollbackResuming(suspendPagesContext(t), intent, now.Add(time.Minute), func(context.Context, reopenAttemptIdentity) error { calls++; return nil })
			if err != nil || out.Generation != intent.Generation {
				t.Fatal("retry reused generation or failed to reconcile")
			}
			if phase == "directory-sync" && (calls != before || out.UpdatedAt != current.UpdatedAt || !reflect.DeepEqual(out.Suspension, current.Suspension)) {
				t.Fatal("visible retry repeated cleanup or refreshed receipt/TTL")
			}
		})
	}
}

func TestSuspendRollbackReopenRejectsBackdatedCommit(t *testing.T) {
	s, _, _, intent, now := suspendRollbackFixture(t)
	rolled, err := s.rollbackResuming(suspendPagesContext(t), intent, now, func(context.Context, reopenAttemptIdentity) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	beforeReceipt := now.Add(-time.Nanosecond)
	planned, err := planReopenRecord(rolled, rolled.OwnerUserID, false, beforeReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.stageResuming(suspendPagesContext(t), rolled, planned, beforeReceipt, func() error { return nil }); err == nil {
		t.Fatal("reopen committed a timestamp before its recorded rollback")
	}
	current, err := s.read(rolled.ID)
	if err != nil || !reflect.DeepEqual(current, rolled) {
		t.Fatal("backdated reopen replaced the valid rollback receipt")
	}
}
