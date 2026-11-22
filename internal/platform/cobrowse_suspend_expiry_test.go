package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func suspendExpiryFixture(t *testing.T, persistent bool) (*suspendIntentStore, string, sessionRecord, time.Time) {
	t.Helper()
	s, dir, _, _ := suspendIntentFixture(t)
	r, capture := captureRecord(t, suspendTestManifest(), persistent, &fakeSuspendSealer{})
	if err := s.payloads.write(r, capture); err != nil {
		t.Fatal(err)
	}
	if err := saveRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	r, err := s.read(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	expiry, err := suspendExpiry(r)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir, r, expiry
}

func TestSuspendExpiryClockAndProfileRetention(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "throwaway", true: "persistent"}[persistent], func(t *testing.T) {
			s, dir, r, expiry := suspendExpiryFixture(t, persistent)
			// A synthetic, separate profile artifact must remain byte-identical.
			profile := filepath.Join(filepath.Dir(dir), "kept-profile.fixture")
			if err := os.WriteFile(profile, []byte("synthetic saved profile bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			before, due, err := s.expire(suspendPagesContext(t), r, expiry.Add(-time.Nanosecond))
			if err != nil || due || !reflect.DeepEqual(before, r) {
				t.Fatal("session expired before its TTL")
			}
			name, _ := suspendManifestName(r)
			if _, err := s.payloads.root.Stat(name); err != nil {
				t.Fatal("early sweep removed tabs")
			}
			var order []string
			payloadSync, fileSync, rename, dirSync := s.payloads.syncDir, s.syncFile, s.rename, s.syncDir
			s.payloads.syncDir = func() error { order = append(order, "payload-removed"); return payloadSync() }
			s.syncFile = func(f *os.File) error { order = append(order, "record-flushed"); return fileSync(f) }
			s.rename = func(a, b string) error { order = append(order, "record-published"); return rename(a, b) }
			s.syncDir = func() error { order = append(order, "directory-flushed"); return dirSync() }
			after, due, err := s.expire(suspendPagesContext(t), r, expiry)
			if err != nil || !due || after.State != RecordExpired || after.EndedReason != "suspend_expired" ||
				after.EndedAt != expiry.UTC().Format(time.RFC3339Nano) || after.Suspension.TabCount != 0 || after.Suspension.ManifestBytes != 0 {
				t.Fatal("expiry did not publish its bounded tombstone at the original TTL")
			}
			if !reflect.DeepEqual(order, []string{"payload-removed", "record-flushed", "record-published", "directory-flushed"}) {
				t.Fatal("tombstone committed before durable tab removal")
			}
			if _, err := s.payloads.root.Stat(name); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("expired tab payload still exists")
			}
			if data, err := os.ReadFile(profile); err != nil || string(data) != "synthetic saved profile bytes" {
				t.Fatal("expiry altered a saved profile")
			}
			committed, err := loadRecord(dir, r.ID)
			if err != nil || !reflect.DeepEqual(committed, after) {
				t.Fatal("expiry acknowledged a different record")
			}
			again, due, err := s.expire(suspendPagesContext(t), after, expiry.Add(time.Hour))
			if err != nil || due || again.EndedAt != after.EndedAt {
				t.Fatal("repeat expiry refreshed the tombstone timestamp")
			}
		})
	}
}

func TestSuspendExpiryFailureReplay(t *testing.T) {
	for _, phase := range []string{"payload-sync", "record-sync", "rename", "directory-sync", "deadline-after-delete"} {
		t.Run(phase, func(t *testing.T) {
			s, dir, r, expiry := suspendExpiryFixture(t, false)
			ctx, cancel := context.WithCancel(suspendPagesContext(t))
			defer cancel()
			payloadSync, fileSync, rename, dirSync := s.payloads.syncDir, s.syncFile, s.rename, s.syncDir
			refusal := errors.New("private-sentinel")
			switch phase {
			case "payload-sync":
				s.payloads.syncDir = func() error { return refusal }
			case "record-sync":
				s.syncFile = func(*os.File) error { return refusal }
			case "rename":
				s.rename = func(string, string) error { return refusal }
			case "directory-sync":
				s.syncDir = func() error { return refusal }
			case "deadline-after-delete":
				s.payloads.syncDir = func() error { cancel(); return payloadSync() }
			}
			if out, due, err := s.expire(ctx, r, expiry); err == nil || due || out.ID != "" || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("failed expiry acknowledged success or exposed provider text")
			}
			s.payloads.syncDir, s.syncFile, s.rename, s.syncDir = payloadSync, fileSync, rename, dirSync
			current, err := s.read(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantState := RecordSuspended
			if phase == "directory-sync" {
				wantState = RecordExpired
			}
			if current.State != wantState {
				t.Fatal("incorrect crash-visible expiry state")
			}
			replayed, _, err := s.expire(suspendPagesContext(t), current, expiry.Add(time.Minute))
			if err != nil || replayed.State != RecordExpired || replayed.EndedAt != expiry.UTC().Format(time.RFC3339Nano) {
				t.Fatal("interrupted expiry could not reconcile without refreshing TTL")
			}
			if committed, err := loadRecord(dir, r.ID); err != nil || committed.State != RecordExpired {
				t.Fatal("replay did not retain a tombstone")
			}
		})
	}
}

func TestSuspendExpiryReconcilesVisibleTombstone(t *testing.T) {
	s, _, r, expiry := suspendExpiryFixture(t, false)
	syncDir := s.syncDir
	s.syncDir = func() error { return errors.New("synthetic flush failure") }
	if _, _, err := s.expire(suspendPagesContext(t), r, expiry); err == nil {
		t.Fatal("first flush failure was acknowledged")
	}
	current, err := s.read(r.ID)
	if err != nil || current.State != RecordExpired {
		t.Fatal("missing uncertain tombstone")
	}
	if _, _, err := s.expire(suspendPagesContext(t), current, expiry.Add(time.Hour)); err == nil {
		t.Fatal("visible tombstone was mistaken for durable success")
	}
	flushes := 0
	s.syncDir = func() error { flushes++; return syncDir() }
	reconciled, due, err := s.expire(suspendPagesContext(t), current, expiry.Add(2*time.Hour))
	if err != nil || due || flushes != 1 || reconciled.EndedAt != current.EndedAt {
		t.Fatal("retry failed to flush the original tombstone without refreshing it")
	}
}

func TestSuspendExpiryKeepsSiblingPayload(t *testing.T) {
	s, _, r, expiry := suspendExpiryFixture(t, false)
	m := suspendTestManifest()
	m.SessionID = "cb-beef"
	sibling, capture := captureRecord(t, m, false, nil)
	if err := s.payloads.write(sibling, capture); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.expire(suspendPagesContext(t), r, expiry); err != nil {
		t.Fatal(err)
	}
	got, err := s.payloads.read(sibling, nil)
	if err != nil || len(got.Tabs) != len(m.Tabs) {
		t.Fatal("expiring one session removed its sibling's tabs")
	}
}

func TestSuspendExpiryRefusalsLeavePayload(t *testing.T) {
	for _, kind := range []string{"no-deadline", "cancelled", "zero-clock", "stale-generation", "malformed-peer", "payload-object"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, r, expiry := suspendExpiryFixture(t, false)
			ctx := suspendPagesContext(t)
			expected := r
			switch kind {
			case "no-deadline":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "zero-clock":
				expiry = time.Time{}
			case "stale-generation":
				expected.Generation++
			case "malformed-peer":
				if err := os.WriteFile(filepath.Join(dir, "cb-dead.json"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "payload-object":
				name, _ := suspendManifestName(r)
				if err := s.payloads.root.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := s.payloads.root.Mkdir(name, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if _, due, err := s.expire(ctx, expected, expiry); err == nil || due {
				t.Fatal("invalid expiry admitted a destructive transition")
			}
			current, err := s.read(r.ID)
			if err != nil || current.State != RecordSuspended {
				t.Fatal("refused expiry replaced the registry state")
			}
			name, _ := suspendManifestName(r)
			if _, err := s.payloads.root.Lstat(name); err != nil {
				t.Fatal("refused expiry removed its payload")
			}
		})
	}
}

func TestSuspendTombstoneRetentionAndIdempotentPrune(t *testing.T) {
	s, dir, r, expiry := suspendExpiryFixture(t, false)
	ended, _, err := s.expire(suspendPagesContext(t), r, expiry)
	if err != nil {
		t.Fatal(err)
	}
	deadline := expiry.Add(7 * 24 * time.Hour)
	if removed, err := s.pruneTombstone(suspendPagesContext(t), ended, deadline.Add(-time.Nanosecond)); err != nil || removed {
		t.Fatal("tombstone was removed before seven days")
	}
	if _, err := s.read(r.ID); err != nil {
		t.Fatal("retained tombstone disappeared")
	}
	syncDir := s.syncDir
	s.syncDir = func() error { return errors.New("private-sentinel") }
	if removed, err := s.pruneTombstone(suspendPagesContext(t), ended, deadline); err == nil || removed || strings.Contains(err.Error(), "private-sentinel") {
		t.Fatal("uncertain prune acknowledged success")
	}
	if _, err := os.Stat(recordFile(dir, r.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("prune did not reach its expected uncertain absence")
	}
	if removed, err := s.pruneTombstone(suspendPagesContext(t), ended, deadline); err == nil || removed {
		t.Fatal("absence alone was acknowledged while its directory flush still failed")
	}
	s.syncDir = syncDir
	if removed, err := s.pruneTombstone(suspendPagesContext(t), ended, deadline); err != nil || !removed {
		t.Fatal("prune could not flush and acknowledge its prior removal")
	}
	if removed, err := s.pruneTombstone(suspendPagesContext(t), ended, deadline.Add(time.Hour)); err != nil || !removed {
		t.Fatal("repeated prune was not idempotent")
	}
}

func TestSuspendTombstoneOtherReasonsAndDeleteFailures(t *testing.T) {
	for _, kind := range []string{"unrelated-end", "stale-record", "remove-failed", "no-deadline", "zero-clock", "invalid-id"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, r, expiry := suspendExpiryFixture(t, false)
			ended, _, err := s.expire(suspendPagesContext(t), r, expiry)
			if err != nil {
				t.Fatal(err)
			}
			expected := ended
			ctx, now := suspendPagesContext(t), expiry.Add(suspendTombstoneTTL)
			switch kind {
			case "unrelated-end":
				ended.EndedReason = "unrelated"
				if err := saveRecord(dir, ended); err != nil {
					t.Fatal(err)
				}
				expected, err = s.read(r.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "stale-record":
				expected.Generation++
			case "remove-failed":
				s.removeRecord = func(string) error { return errors.New("private-sentinel") }
			case "no-deadline":
				ctx = context.Background()
			case "zero-clock":
				now = time.Time{}
			case "invalid-id":
				expected.ID = "../outside"
			}
			removed, err := s.pruneTombstone(ctx, expected, now)
			if removed || (kind != "unrelated-end" && err == nil) || (err != nil && strings.Contains(err.Error(), "private-sentinel")) {
				t.Fatal("invalid or failed prune acknowledged deletion")
			}
			if _, err := s.read(r.ID); err != nil {
				t.Fatal("refused prune removed the tombstone")
			}
		})
	}
}
