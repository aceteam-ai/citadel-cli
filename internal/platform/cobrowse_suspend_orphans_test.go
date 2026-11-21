package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func suspendOrphanFixture(t *testing.T, n int) (*suspendIntentStore, []string) {
	t.Helper()
	s, _, _ := suspendSweepFixture(t, 0)
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("cb-%03x.1.tabs", i)
		if err := os.WriteFile(filepath.Join(s.payloads.root.Name(), names[i]), []byte("synthetic-tab-payload"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return s, names
}

func TestSuspendOrphansBoundedProgressAndDurability(t *testing.T) {
	if maxSuspendOrphanRemovals != 16 {
		t.Fatal("orphan effect budget changed without a contract update")
	}
	s, names := suspendOrphanFixture(t, 35)
	var order []string
	remove, flush := s.payloads.removeOrphan, s.payloads.syncDir
	s.payloads.removeOrphan = func(name string) error { order = append(order, "remove"); return remove(name) }
	s.payloads.syncDir = func() error { order = append(order, "flush"); return flush() }
	cursor := ""
	for _, want := range []int{16, 16, 3, 0} {
		order = nil
		result, err := s.sweepOrphanPayloads(suspendPagesContext(t), cursor)
		if err != nil || result.Attempted != want || result.Removed != want || result.Failed != 0 {
			t.Fatalf("bounded cleanup lost progress: %+v, %v", result, err)
		}
		wantOrder := []string{"flush"}
		for range want {
			wantOrder = append(wantOrder, "remove", "flush")
		}
		if !reflect.DeepEqual(order, wantOrder) {
			t.Fatal("removal was acknowledged before directory durability")
		}
		cursor = result.Cursor
	}
	for _, name := range names {
		if _, err := s.payloads.root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("orphan was starved")
		}
	}
	if _, err := s.payloads.lockFile.Stat(); err != nil {
		t.Fatal("cleanup removed the process ownership lock")
	}
}

func TestSuspendOrphansPreserveAllReferencedStates(t *testing.T) {
	s, records, _ := suspendSweepFixture(t, 3)
	for i, state := range []string{RecordSuspended, "suspending", "resuming"} {
		records[i].State = state
		if state == "resuming" {
			records[i].Generation++
		}
		if err := saveRecord(s.root.Name(), records[i]); err != nil {
			t.Fatal(err)
		}
	}
	profile := filepath.Join(filepath.Dir(s.root.Name()), "synthetic-profile")
	if err := os.WriteFile(profile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := s.sweepOrphanPayloads(suspendPagesContext(t), "")
	if err != nil || result.Scanned != 3 || result.Attempted != 0 || result.Removed != 0 {
		t.Fatal("referenced current or previous capture generation was treated as orphan")
	}
	for _, rec := range records {
		if _, err := s.payloads.read(rec, nil); err != nil {
			t.Fatal("retained capture was removed")
		}
	}
	if data, err := os.ReadFile(profile); err != nil || string(data) != "keep" {
		t.Fatal("unrelated profile artifact changed")
	}
	// Known interrupted write names are reclaimed, without reading their bytes.
	name := ".tmp-0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(s.payloads.root.Name(), name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = s.sweepOrphanPayloads(suspendPagesContext(t), result.Cursor)
	if err != nil || result.Removed != 1 {
		t.Fatal("known crash temporary file was not reclaimed")
	}
}

func suspendOrphanTombstoneFixture(t *testing.T, persistent, rollback bool) (*suspendIntentStore, sessionRecord, time.Time, string) {
	t.Helper()
	s, _, rec, expiry := suspendExpiryFixture(t, persistent)
	if rollback {
		now := suspendTestNow.Add(time.Minute)
		planned, err := planReopenRecord(rec, rec.OwnerUserID, true, now)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := s.stageResuming(suspendPagesContext(t), rec, planned, now, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		rec, err = s.rollbackResuming(suspendPagesContext(t), intent, now.Add(time.Minute), func(context.Context, reopenAttemptIdentity) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	name, err := suspendManifestName(rec)
	if err != nil {
		t.Fatal(err)
	}
	tombstone, changed, err := s.expire(suspendPagesContext(t), rec, expiry.Add(time.Minute))
	if err != nil || !changed {
		t.Fatal("could not create real TTL tombstone")
	}
	return s, tombstone, expiry, name
}

func TestSuspendOrphansAfterExpiry(t *testing.T) {
	for _, kind := range []struct{ persistent, rollback bool }{{}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("persistent=%t/rollback=%t", kind.persistent, kind.rollback), func(t *testing.T) {
			s, tombstone, expiry, captureName := suspendOrphanTombstoneFixture(t, kind.persistent, kind.rollback)
			profile := filepath.Join(filepath.Dir(s.root.Name()), "retained-profile.fixture")
			if err := os.WriteFile(profile, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			// If an old capture unexpectedly reappears, conservatively protect
			// it until the tombstone is pruned; only unrelated orphans are free.
			if err := os.WriteFile(filepath.Join(s.payloads.root.Name(), captureName), []byte("uncertain old capture"), 0o600); err != nil {
				t.Fatal(err)
			}
			name := "cb-abcd.1.tabs"
			if err := os.WriteFile(filepath.Join(s.payloads.root.Name(), name), []byte("orphan"), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := s.sweepOrphanPayloads(suspendPagesContext(t), "")
			if err != nil || result.Attempted != 1 || result.Removed != 1 {
				t.Fatalf("TTL tombstone blocked unrelated orphan cleanup: %+v, %v", result, err)
			}
			if _, err := s.payloads.root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unrelated orphan was acknowledged without removal")
			}
			if _, err := s.payloads.root.Lstat(captureName); err != nil {
				t.Fatal("tombstone capture was mistaken for unrelated free storage")
			}
			got, err := s.read(tombstone.ID)
			if err != nil || !reflect.DeepEqual(got, tombstone) {
				t.Fatal("orphan cleanup changed tombstone retention")
			}
			if data, err := os.ReadFile(profile); err != nil || string(data) != "keep" {
				t.Fatal("orphan cleanup changed retained profile")
			}
			if removed, err := s.pruneTombstone(suspendPagesContext(t), tombstone, expiry.Add(suspendTombstoneTTL)); err != nil || !removed {
				t.Fatal("could not prune due tombstone")
			}
			if result, err := s.sweepOrphanPayloads(suspendPagesContext(t), result.Cursor); err != nil || result.Removed != 1 {
				t.Fatal("pruned tombstone permanently pinned crash storage")
			}
			if _, err := s.payloads.root.Lstat(captureName); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("pruned capture was acknowledged without removal")
			}
		})
	}
}

func TestSuspendOrphansRefuseMalformedTombstones(t *testing.T) {
	for _, kind := range []string{"reason", "state", "missing-metadata", "tabs", "bytes", "expiry", "refreshed-ttl", "ended", "updated", "before-expiry", "generation", "capture-generation", "rollback-generation", "rollback-time", "rollback-future", "rollback-unrecorded"} {
		t.Run(kind, func(t *testing.T) {
			s, rec, expiry, _ := suspendOrphanTombstoneFixture(t, false, true)
			switch kind {
			case "reason":
				rec.EndedReason = "other"
			case "state":
				rec.State = RecordLive
			case "missing-metadata":
				rec.Suspension = nil
			case "tabs":
				rec.Suspension.TabCount = 1
			case "bytes":
				rec.Suspension.ManifestBytes = 1
			case "expiry":
				rec.Suspension.ExpiresAt = expiry.Add(time.Hour).Format(time.RFC3339Nano)
			case "refreshed-ttl":
				rec.Suspension.ExpiresAt = expiry.Add(time.Hour).Format(time.RFC3339Nano)
				rec.EndedAt = rec.Suspension.ExpiresAt
				rec.UpdatedAt = rec.EndedAt
			case "ended":
				rec.EndedAt = expiry.Add(-time.Minute).Format(time.RFC3339Nano)
			case "updated":
				rec.UpdatedAt = "invalid"
			case "before-expiry":
				rec.UpdatedAt = expiry.Add(-time.Second).Format(time.RFC3339Nano)
			case "generation":
				rec.Generation++
			case "capture-generation":
				rec.Suspension.CaptureGeneration = 0
			case "rollback-generation":
				rec.Suspension.ReopenRollback.Generation = rec.Suspension.CaptureGeneration
			case "rollback-time":
				rec.Suspension.ReopenRollback.At = "invalid"
			case "rollback-future":
				rec.Suspension.ReopenRollback.At = expiry.Add(2 * time.Minute).Format(time.RFC3339Nano)
			case "rollback-unrecorded":
				rec.Suspension.ReopenRollback = nil
			}
			if name, err := suspendTombstoneCaptureName(rec); err == nil || name != "" {
				t.Fatal("malformed terminal metadata produced a capture reference")
			}
			// Legacy saveRecord replaces UpdatedAt with the wall clock. Write
			// the exact synthetic receipt so another invalid timestamp cannot
			// accidentally mask the particular bound under test.
			data, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.root.Name(), rec.ID+".json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			stored, err := s.read(rec.ID)
			if err != nil || !reflect.DeepEqual(stored, rec) {
				t.Fatal("malformed receipt fixture changed before the scan")
			}
			name := "cb-abcd.1.tabs"
			if err := os.WriteFile(filepath.Join(s.payloads.root.Name(), name), []byte("orphan"), 0o600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.payloads.removeOrphan = func(string) error { calls++; return nil }
			result, err := s.sweepOrphanPayloads(suspendPagesContext(t), "kept-cursor")
			if err == nil || calls != 0 || result.Attempted != 0 || result.Removed != 0 || result.Cursor != "kept-cursor" {
				t.Fatal("malformed terminal metadata crossed orphan removal boundary")
			}
		})
	}
}

func TestSuspendOrphansRefuseUncertainScanBeforeDeletion(t *testing.T) {
	for _, kind := range []string{"deadline", "cancelled", "unknown-name", "directory", "oversize", "entry-cap", "registry-invalid", "registry-flush", "payload-flush", "nil-remover"} {
		t.Run(kind, func(t *testing.T) {
			s, names := suspendOrphanFixture(t, 1)
			ctx := suspendPagesContext(t)
			calls := 0
			remove := s.payloads.removeOrphan
			s.payloads.removeOrphan = func(name string) error { calls++; return remove(name) }
			switch kind {
			case "deadline":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "unknown-name":
				if err := os.WriteFile(filepath.Join(s.payloads.root.Name(), "profile.dat"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(filepath.Join(s.payloads.root.Name(), "cb-feed.1.tabs"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.Truncate(filepath.Join(s.payloads.root.Name(), names[0]), maxSuspendManifestBytes+4097); err != nil {
					t.Fatal(err)
				}
			case "entry-cap":
				for i := 0; i < maxSuspendStoreEntries; i++ {
					if err := os.WriteFile(filepath.Join(s.payloads.root.Name(), fmt.Sprintf("cb-%04x.1.tabs", i+4096)), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			case "registry-invalid":
				if err := saveRecord(s.root.Name(), sessionRecord{Version: sessionRecordVersion, ID: "cb-bad", State: "unknown"}); err != nil {
					t.Fatal(err)
				}
			case "registry-flush":
				s.syncDir = func() error { return errors.New("private-orphan-sentinel") }
			case "payload-flush":
				s.payloads.syncDir = func() error { return errors.New("private-orphan-sentinel") }
			case "nil-remover":
				s.payloads.removeOrphan = nil
			}
			result, err := s.sweepOrphanPayloads(ctx, "earned-cursor")
			if err == nil || strings.Contains(err.Error(), "private-orphan-sentinel") || calls != 0 || result.Removed != 0 || result.Cursor != "earned-cursor" {
				t.Fatal("uncertain scan deleted a payload, leaked details or lost its cursor")
			}
			if _, err := s.payloads.root.Lstat(names[0]); err != nil {
				t.Fatal("refused scan removed a candidate")
			}
		})
	}
}

func TestSuspendOrphansFailuresDoNotStarveAndRetryFlushes(t *testing.T) {
	s, names := suspendOrphanFixture(t, 35)
	remove := s.payloads.removeOrphan
	blocked := make(map[string]bool)
	for _, name := range names[:16] {
		blocked[name] = true
	}
	s.payloads.removeOrphan = func(name string) error {
		if blocked[name] {
			return errors.New("private-orphan-sentinel")
		}
		return remove(name)
	}
	cursor := ""
	for range 3 {
		result, err := s.sweepOrphanPayloads(suspendPagesContext(t), cursor)
		if result.Attempted > 16 || (result.Failed > 0 && (err == nil || strings.Contains(err.Error(), "private-orphan-sentinel"))) {
			t.Fatal("partial failure lost budget or opaque error")
		}
		cursor = result.Cursor
	}
	for i, name := range names {
		_, err := s.payloads.root.Lstat(name)
		if (i < 16 && err != nil) || (i >= 16 && !errors.Is(err, os.ErrNotExist)) {
			t.Fatal("blocked entries starved healthy orphans")
		}
	}
	clear(blocked)
	result, err := s.sweepOrphanPayloads(suspendPagesContext(t), cursor)
	if err != nil || result.Removed != 16 {
		t.Fatal("repaired orphans could not finish")
	}
	// An unlink can be visible while its directory sync is uncertain.
	s, _ = suspendOrphanFixture(t, 1)
	flush, calls := s.payloads.syncDir, 0
	s.payloads.syncDir = func() error {
		calls++
		if calls > 1 {
			return errors.New("private-orphan-sentinel")
		}
		return flush()
	}
	result, err = s.sweepOrphanPayloads(suspendPagesContext(t), "")
	if err == nil || result.Removed != 0 || result.Failed != 1 {
		t.Fatal("uncertain unlink was acknowledged")
	}
	if _, err := s.sweepOrphanPayloads(suspendPagesContext(t), result.Cursor); err == nil {
		t.Fatal("empty retry skipped missing-entry durability")
	}
	s.payloads.syncDir = flush
	if result, err = s.sweepOrphanPayloads(suspendPagesContext(t), result.Cursor); err != nil || result.Removed != 0 {
		t.Fatal("empty retry did not reconcile durability")
	}
}

func TestSuspendOrphansRecheckReferencesAndFileIdentity(t *testing.T) {
	for _, kind := range []string{"new-reference", "replacement", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s, records, _ := suspendSweepFixture(t, 1)
			rec := records[0]
			if err := s.root.Remove(rec.ID + ".json"); err != nil {
				t.Fatal(err)
			}
			name, _ := suspendManifestName(rec)
			ctx, cancel := context.WithCancel(suspendPagesContext(t))
			defer cancel()
			flush, remove := s.payloads.syncDir, s.payloads.removeOrphan
			calls := 0
			s.payloads.removeOrphan = func(name string) error { calls++; return remove(name) }
			s.payloads.syncDir = func() error {
				switch kind {
				case "new-reference":
					if err := saveRecord(s.root.Name(), rec); err != nil {
						return err
					}
				case "replacement":
					// Same size and mode, distinct inode: only identity catches it.
					tmp := filepath.Join(s.payloads.root.Name(), ".replacement")
					if err := os.WriteFile(tmp, []byte(strings.Repeat("x", int(rec.Suspension.ManifestBytes))), 0o600); err != nil {
						return err
					}
					if err := os.Rename(tmp, filepath.Join(s.payloads.root.Name(), name)); err != nil {
						return err
					}
				case "cancel":
					cancel()
				}
				return flush()
			}
			result, err := s.sweepOrphanPayloads(ctx, "")
			if err == nil || calls != 0 || result.Removed != 0 {
				t.Fatal("changed reference, object or budget crossed unlink boundary")
			}
			if _, err := s.payloads.root.Lstat(name); err != nil {
				t.Fatal("changed capture was removed")
			}
		})
	}
}

func TestSuspendOrphansCanonicalNames(t *testing.T) {
	for name, want := range map[string]bool{
		"cb-abc.1.tabs": true, "cb-abc.18446744073709551615.tabs": true,
		".tmp-0123456789abcdef0123456789abcdef": true,
		"cb-abc.01.tabs":                        false, "cb-abc.0.tabs": false, "cb-abc.+1.tabs": false,
		"cb-abc.18446744073709551616.tabs": false, "../cb-abc.1.tabs": false,
		"cb-abc.1.tabs.extra": false, ".tmp-short": false,
		".tmp-0123456789ABCDEF0123456789ABCDEF": false, "profile.dat": false,
	} {
		if suspendPayloadName(name) != want {
			t.Fatal("name outside the store's canonical namespace was accepted")
		}
	}
}

func TestSuspendOrphansFilesystemRefusals(t *testing.T) {
	for _, kind := range []string{"byte-cap", "symlink", "non-private", "detached-directory", "uncertain-live-reference"} {
		t.Run(kind, func(t *testing.T) {
			if runtime.GOOS == "windows" && (kind == "symlink" || kind == "non-private" || kind == "detached-directory") {
				t.Skip("POSIX permissions, symlink or open-directory rename proof")
			}
			s, names := suspendOrphanFixture(t, 1)
			path := filepath.Join(s.payloads.root.Name(), names[0])
			switch kind {
			case "byte-cap":
				for i := 0; i < 33; i++ {
					f, err := s.payloads.root.OpenFile(fmt.Sprintf("cb-%04x.1.tabs", i+4096), os.O_CREATE|os.O_WRONLY, 0o600)
					if err != nil {
						t.Fatal(err)
					}
					err = f.Truncate(maxSuspendManifestBytes + 4096)
					_ = f.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
			case "symlink":
				if err := os.Symlink(path, filepath.Join(s.payloads.root.Name(), "cb-feed.1.tabs")); err != nil {
					t.Fatal(err)
				}
			case "non-private":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "detached-directory":
				if err := os.Rename(s.payloads.root.Name(), s.payloads.root.Name()+"-detached"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(s.payloads.root.Name(), 0o700); err != nil {
					t.Fatal(err)
				}
			case "uncertain-live-reference":
				m := suspendTestManifest()
				r, _ := captureRecord(t, m, false, nil)
				r.State = RecordLive // A malformed legacy reference must not free its bytes.
				if err := saveRecord(s.root.Name(), r); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			s.payloads.removeOrphan = func(string) error { calls++; return nil }
			result, err := s.sweepOrphanPayloads(suspendPagesContext(t), "earned-cursor")
			if err == nil || calls != 0 || result.Removed != 0 || result.Cursor != "earned-cursor" {
				t.Fatal("uncertain storage crossed the orphan removal boundary")
			}
			if _, err := s.payloads.root.Lstat(names[0]); err != nil {
				t.Fatal("refusal removed original payload")
			}
		})
	}
}
