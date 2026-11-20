package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func suspendSweepFixture(t *testing.T, n int) (*suspendIntentStore, []sessionRecord, time.Time) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "registry")
	s, err := openSuspendIntentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.close() })
	var records []sessionRecord
	for i := 0; i < n; i++ {
		m := suspendTestManifest()
		m.SessionID = fmt.Sprintf("cb-%x", i+256)
		r, p := captureRecord(t, m, false, nil)
		r.OwnerUserID = fmt.Sprintf("synthetic-user-%d", i)
		r.Suspension.OwnerUserID = r.OwnerUserID
		if err := s.payloads.write(r, p); err != nil {
			t.Fatal(err)
		}
		if err := saveRecord(dir, r); err != nil {
			t.Fatal(err)
		}
		r, err = s.read(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return s, records, suspendTestNow.Add(24 * time.Hour)
}

func TestSuspendTTLSweepBoundedLevelTriggeredPasses(t *testing.T) {
	if maxSuspendTTLTransitions != 16 {
		t.Fatal("TTL transition budget changed without a contract update")
	}
	s, records, now := suspendSweepFixture(t, 35)
	cursor := ""
	for i, want := range []int{16, 16, 3, 0} {
		result, err := s.sweepTTL(suspendPagesContext(t), now, cursor)
		if err != nil || result.Attempted != want || result.Expired != want || result.Failed != 0 || result.Pruned != 0 {
			t.Fatalf("expiry pass %d did not respect its bounded budget: %+v", i, result)
		}
		cursor = result.Cursor
	}
	for _, r := range records {
		got, err := s.read(r.ID)
		if err != nil || got.State != RecordExpired {
			t.Fatal("due session was starved by the batch cap")
		}
	}
	for i, want := range []int{16, 16, 3, 0} {
		result, err := s.sweepTTL(suspendPagesContext(t), now.Add(7*24*time.Hour), cursor)
		if err != nil || result.Attempted != want || result.Pruned != want || result.Failed != 0 || result.Expired != 0 {
			t.Fatalf("prune pass %d did not respect its bounded budget: %+v", i, result)
		}
		cursor = result.Cursor
	}
	for _, r := range records {
		if _, err := s.root.Lstat(r.ID + ".json"); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("expired tombstone was not pruned")
		}
	}
}

func TestSuspendTTLSweepFailedEntryDoesNotStarveSiblings(t *testing.T) {
	s, records, now := suspendSweepFixture(t, 35)
	rename := s.rename
	blocked := make(map[string]bool)
	for _, r := range records[:maxSuspendTTLTransitions] {
		blocked[r.ID+".json"] = true
	}
	s.rename = func(a, b string) error {
		if blocked[b] {
			return errors.New("synthetic filesystem refusal")
		}
		return rename(a, b)
	}
	cursor := ""
	for i := 0; i < 3; i++ {
		result, err := s.sweepTTL(suspendPagesContext(t), now, cursor)
		if result.Attempted > 16 || (result.Failed > 0 && err == nil) {
			t.Fatal("partial failure lost its budget or error")
		}
		cursor = result.Cursor // progress survives the returned partial error
	}
	for i, r := range records {
		got, err := s.read(r.ID)
		if err != nil || (i < maxSuspendTTLTransitions && got.State != RecordSuspended) || (i >= maxSuspendTTLTransitions && got.State != RecordExpired) {
			t.Fatal("failed entry starved an otherwise valid sibling")
		}
	}
	s.rename = rename
	result, err := s.sweepTTL(suspendPagesContext(t), now, cursor)
	if err != nil || result.Expired != maxSuspendTTLTransitions || result.Failed != 0 {
		t.Fatal("repaired entry could not finish on a later pass")
	}
}

func TestSuspendTTLSweepBudgetAndEmptyRegistry(t *testing.T) {
	s, _, now := suspendSweepFixture(t, 0)
	if result, err := s.sweepTTL(suspendPagesContext(t), now, ""); err != nil || result.Scanned != 0 || result.Attempted != 0 {
		t.Fatal("empty sweep was not a no-op")
	}
	if _, err := s.sweepTTL(context.Background(), now, ""); err == nil {
		t.Fatal("unbounded sweep was admitted")
	}
	if _, err := s.sweepTTL(suspendPagesContext(t), time.Time{}, ""); err == nil {
		t.Fatal("sweep without a clock was admitted")
	}
}

func TestSuspendTTLSweepFlushesUncertainMetadataOnNextPass(t *testing.T) {
	s, records, now := suspendSweepFixture(t, 1)
	syncDir := s.syncDir
	flushes := 0
	s.syncDir = func() error {
		flushes++
		if flushes == 2 {
			return errors.New("synthetic interrupted flush")
		}
		return syncDir()
	}
	result, err := s.sweepTTL(suspendPagesContext(t), now, "")
	if err == nil || result.Failed != 1 {
		t.Fatal("interrupted transition was acknowledged")
	}
	r, err := s.read(records[0].ID)
	if err != nil || r.State != RecordExpired {
		t.Fatal("missing uncertain expired record")
	}
	s.syncDir = func() error { return errors.New("synthetic persistent flush failure") }
	if _, err := s.sweepTTL(suspendPagesContext(t), now, result.Cursor); err == nil {
		t.Fatal("visible tombstone bypassed durability reconciliation")
	}
	flushes = 0
	s.syncDir = func() error { flushes++; return syncDir() }
	if result, err := s.sweepTTL(suspendPagesContext(t), now, ""); err != nil || result.Attempted != 0 || flushes != 1 {
		t.Fatal("retry did not reconcile metadata in one bounded flush")
	}
}
