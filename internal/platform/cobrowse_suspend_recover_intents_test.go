package platform

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func suspendRecoveryPassFixture(t *testing.T, n int) (*suspendIntentStore, []sessionRecord, time.Time) {
	t.Helper()
	s, records, _ := suspendSweepFixture(t, n)
	for i, rec := range records {
		rec.State = "suspending"
		rec.BrowserID = "synthetic-browser-" + rec.ID
		rec.BrowserPID = 900000 + i // Value-only facts; never passed to an OS killer.
		rec.DebugPort = 20000 + i
		if err := saveRecord(s.root.Name(), rec); err != nil {
			t.Fatal(err)
		}
		var err error
		records[i], err = s.read(rec.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	return s, records, suspendTestNow.Add(time.Hour)
}

func TestSuspendRecoveryPassBoundedRestartAndRetry(t *testing.T) {
	if maxSuspendRecoveryTransitions != 16 {
		t.Fatal("recovery transition budget changed without a contract update")
	}
	s, records, now := suspendRecoveryPassFixture(t, 35)
	seen := make(map[string]int)
	want := make(map[string]suspendStopIdentity)
	for _, rec := range records {
		want[rec.ID] = suspendIdentity(rec)
	}
	stop := func(ctx context.Context, identity suspendStopIdentity) error {
		if _, finite := ctx.Deadline(); !finite || want[identity.SessionID] != identity {
			t.Error("recovery lost the exact process facts or caller budget")
		}
		seen[identity.SessionID]++
		return nil
	}
	cursor := ""
	for pass, budget := range []int{16, 16, 3, 0} {
		result, err := s.recoverSuspending(suspendPagesContext(t), now, cursor, stop)
		if err != nil || result.Scanned != 35 || result.Attempted != budget || result.Completed != budget || result.Failed != 0 {
			t.Fatalf("recovery pass %d exceeded its effect budget or lost progress: %+v", pass, result)
		}
		cursor = result.Cursor
	}
	for _, rec := range records {
		current, err := s.read(rec.ID)
		if err != nil || current.State != RecordSuspended || seen[rec.ID] != 1 ||
			!reflect.DeepEqual(current.Suspension, rec.Suspension) || current.BrowserPID != 0 {
			t.Fatal("restart recovery did not retain exactly one completed capture")
		}
	}
}

func TestSuspendRecoveryPassFailuresDoNotStarveSiblings(t *testing.T) {
	s, records, now := suspendRecoveryPassFixture(t, 35)
	blocked := make(map[string]bool)
	for _, rec := range records[:maxSuspendRecoveryTransitions] {
		blocked[rec.ID] = true
	}
	stop := func(_ context.Context, identity suspendStopIdentity) error {
		if blocked[identity.SessionID] {
			return errors.New("private-provider-sentinel")
		}
		return nil
	}
	cursor := ""
	for range 3 {
		result, err := s.recoverSuspending(suspendPagesContext(t), now, cursor, stop)
		if result.Attempted > 16 || (result.Failed > 0 && (err == nil || strings.Contains(err.Error(), "private-provider-sentinel"))) {
			t.Fatal("recovery lost its budget, partial failure or opaque error")
		}
		cursor = result.Cursor
	}
	for i, rec := range records {
		current, err := s.read(rec.ID)
		want := RecordSuspended
		if i < maxSuspendRecoveryTransitions {
			want = "suspending"
		}
		if err != nil || current.State != want {
			t.Fatal("failed recovery entries starved otherwise healthy siblings")
		}
	}
	clear(blocked)
	result, err := s.recoverSuspending(suspendPagesContext(t), now, cursor, stop)
	if err != nil || result.Completed != 16 || result.Failed != 0 {
		t.Fatal("repaired pending intents could not finish on a later pass")
	}
}

func TestSuspendRecoveryPassAdmissionAndOtherStates(t *testing.T) {
	for _, kind := range []string{"no-deadline", "cancelled", "zero-clock", "nil-stop", "scan-failure", "flush-failure"} {
		t.Run(kind, func(t *testing.T) {
			s, records, now := suspendRecoveryPassFixture(t, 1)
			ctx := suspendPagesContext(t)
			calls, flushes := 0, 0
			syncDir := s.syncDir
			s.syncDir = func() error { flushes++; return syncDir() }
			stop := func(context.Context, suspendStopIdentity) error { calls++; return nil }
			switch kind {
			case "no-deadline":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "zero-clock":
				now = time.Time{}
			case "nil-stop":
				stop = nil
			case "scan-failure":
				if err := saveRecord(s.root.Name(), sessionRecord{Version: sessionRecordVersion, ID: "cb-bad", State: "unrecognized"}); err != nil {
					t.Fatal(err)
				}
			case "flush-failure":
				s.syncDir = func() error { return errors.New("private-provider-sentinel") }
			}
			if _, err := s.recoverSuspending(ctx, now, "", stop); err == nil || calls != 0 || strings.Contains(err.Error(), "private-provider-sentinel") {
				t.Fatal("invalid recovery admission crossed the teardown boundary")
			}
			if kind != "flush-failure" && flushes != 0 {
				t.Fatal("invalid admission performed a registry durability effect")
			}
			current, err := s.read(records[0].ID)
			if err != nil || current.State != "suspending" {
				t.Fatal("refused pass changed a pending intent")
			}
		})
	}
	s, records, now := suspendRecoveryPassFixture(t, 3)
	for i, state := range []string{RecordSuspended, "resuming", RecordLive} {
		records[i].State = state
		if state == "resuming" {
			records[i].Generation++ // The live generation advances, not the capture.
		}
		if err := saveRecord(s.root.Name(), records[i]); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.recoverSuspending(suspendPagesContext(t), now, "", func(context.Context, suspendStopIdentity) error {
		t.Error("recovery inferred a stop for a non-suspending state")
		return nil
	})
	if err != nil || result.Attempted != 0 || result.Completed != 0 || result.Scanned != 3 {
		t.Fatal("recovery did not skip unrelated lifecycle states")
	}
}

func TestSuspendRecoveryPassCancellationPreservesCursor(t *testing.T) {
	s, records, now := suspendRecoveryPassFixture(t, 3)
	ctx, cancel := context.WithCancel(suspendPagesContext(t))
	defer cancel()
	result, err := s.recoverSuspending(ctx, now, "", func(context.Context, suspendStopIdentity) error {
		cancel()
		return ctx.Err()
	})
	if err == nil || result.Attempted != 1 || result.Failed != 1 || result.Cursor != records[0].ID {
		t.Fatal("cancelled recovery lost its bounded partial progress")
	}
	var seen []string
	result, err = s.recoverSuspending(suspendPagesContext(t), now, result.Cursor,
		func(_ context.Context, identity suspendStopIdentity) error {
			seen = append(seen, identity.SessionID)
			return nil
		})
	if err != nil || result.Completed != 3 || fmt.Sprint(seen) != fmt.Sprint([]string{records[1].ID, records[2].ID, records[0].ID}) {
		t.Fatal("next pass did not resume after the cancelled entry")
	}
}

func TestSuspendRecoveryPassReconcilesVisibleMetadata(t *testing.T) {
	s, records, now := suspendRecoveryPassFixture(t, 1)
	syncDir := s.syncDir
	flushes, calls := 0, 0
	s.syncDir = func() error {
		flushes++
		if flushes == 2 {
			return errors.New("synthetic interrupted commit flush")
		}
		return syncDir()
	}
	stop := func(context.Context, suspendStopIdentity) error { calls++; return nil }
	result, err := s.recoverSuspending(suspendPagesContext(t), now, "", stop)
	if err == nil || result.Failed != 1 || calls != 1 {
		t.Fatal("interrupted recovery commit was acknowledged")
	}
	current, err := s.read(records[0].ID)
	if err != nil || current.State != RecordSuspended {
		t.Fatal("missing crash-visible completion")
	}
	s.syncDir = func() error { return errors.New("synthetic persistent flush failure") }
	if _, err := s.recoverSuspending(suspendPagesContext(t), now, result.Cursor, stop); err == nil || calls != 1 {
		t.Fatal("visible completion bypassed recovery reconciliation")
	}
	flushes = 0
	s.syncDir = func() error { flushes++; return syncDir() }
	result, err = s.recoverSuspending(suspendPagesContext(t), now, "", stop)
	after, readErr := s.read(current.ID)
	if err != nil || result.Attempted != 0 || flushes != 1 || calls != 1 || readErr != nil || !reflect.DeepEqual(current, after) {
		t.Fatal("recovery repeated teardown or refreshed already-visible metadata")
	}
}

func TestSuspendRecoveryPassMissingCaptureRemainsRecoverable(t *testing.T) {
	s, records, now := suspendRecoveryPassFixture(t, 3)
	name, err := suspendManifestName(records[0])
	if err != nil || s.payloads.root.Remove(name) != nil {
		t.Fatal("could not prepare synthetic missing payload")
	}
	var calls []string
	result, err := s.recoverSuspending(suspendPagesContext(t), now, "", func(_ context.Context, identity suspendStopIdentity) error {
		calls = append(calls, identity.SessionID)
		return nil
	})
	if err == nil || result.Attempted != 3 || result.Failed != 1 || result.Completed != 2 ||
		!reflect.DeepEqual(calls, []string{records[1].ID, records[2].ID}) {
		t.Fatal("missing capture was silently completed or blocked healthy siblings")
	}
	current, err := s.read(records[0].ID)
	if err != nil || !reflect.DeepEqual(current, records[0]) {
		t.Fatal("missing capture caused an irreversible terminal decision")
	}
}

func TestSuspendRecoveryPassKeepsCursorOnInitialFailure(t *testing.T) {
	for _, kind := range []string{"deadline", "scan", "flush"} {
		t.Run(kind, func(t *testing.T) {
			s, records, now := suspendRecoveryPassFixture(t, 1)
			ctx := suspendPagesContext(t)
			switch kind {
			case "deadline":
				ctx = context.Background()
			case "scan":
				if err := saveRecord(s.root.Name(), sessionRecord{Version: sessionRecordVersion, ID: "cb-bad", State: "unknown"}); err != nil {
					t.Fatal(err)
				}
			case "flush":
				s.syncDir = func() error { return errors.New("synthetic flush failure") }
			}
			result, err := s.recoverSuspending(ctx, now, records[0].ID, func(context.Context, suspendStopIdentity) error {
				t.Error("initial recovery failure invoked teardown")
				return nil
			})
			if err == nil || result.Cursor != records[0].ID || result.Attempted != 0 {
				t.Fatal("initial failure rewound an already-earned recovery cursor")
			}
		})
	}
}
