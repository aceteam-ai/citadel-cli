package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The real legacy recovery pass, with synthetic registry records only. A
// lifecycle transition occurs while its stale deletion is waiting for the gate.
func TestSuspendRegistrySnapshotPrunePreservesNewerRecord(t *testing.T) {
	for _, kind := range []string{"age", "count"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			m := NewCobrowseSessionManager(base, 4)
			m.useResolver = true
			// Keep this manager isolated from the node-wide resolver.
			resolver := registryDirResolver
			registryDirResolver = nil
			t.Cleanup(func() { registryDirResolver = resolver })
			dir := m.registryDir()
			now := time.Now().UTC()
			age := now.Add(-25 * time.Hour)
			if kind == "count" {
				age = now.Add(-time.Hour)
				for i := 0; i < expiredMaxRecords; i++ {
					r := sessionRecord{Version: sessionRecordVersion, ID: fmt.Sprintf("cb-%04x", i+256), State: RecordExpired, EndedAt: now.Format(time.RFC3339)}
					if err := saveRecord(dir, r); err != nil {
						t.Fatal(err)
					}
				}
			}
			original := sessionRecord{Version: sessionRecordVersion, ID: "cb-0001", State: RecordExpired, Generation: 1, EndedAt: age.Format(time.RFC3339)}
			if err := saveRecord(dir, original); err != nil {
				t.Fatal(err)
			}
			release, err := acquireBrowserRegistryGate(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			done := make(chan error, 1)
			go func() { m.recoverSessions(); done <- nil }()
			waiting, consumed := waitBrowserRegistryWriter(t, dir, done)
			// This is the transaction which already owns the gate, not an
			// independent writer bypassing exclusion.
			next := original
			next.State, next.Generation = RecordSuspended, 2
			if err := saveRecordUnderRegistryGate(dir, next); err != nil {
				t.Fatal(err)
			}
			expected, err := loadRecord(dir, next.ID)
			if err != nil {
				t.Fatal(err)
			}
			release()
			if !consumed {
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("pruning did not release its registry gate")
				}
			}
			got, err := loadRecord(dir, next.ID)
			if !waiting || err != nil || !reflect.DeepEqual(got, expected) {
				t.Fatal("stale retention snapshot deleted a newer lifecycle record")
			}
		})
	}
}

func TestSuspendRegistrySnapshotMutation(t *testing.T) {
	for _, kind := range []string{"replace", "delete"} {
		for _, change := range []string{"unchanged", "state", "generation", "metadata", "removed", "malformed", "wrong-id", "wrong-version", "next-version"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "registry")
				original := sessionRecord{Version: sessionRecordVersion, ID: "cb-abcd", State: RecordExpired, Generation: 4}
				if err := saveRecord(dir, original); err != nil {
					t.Fatal(err)
				}
				original, err := loadRecord(dir, original.ID)
				if err != nil {
					t.Fatal(err)
				}
				next := original
				next.State, next.Generation = RecordSuspended, 5
				next.UpdatedAt = "proposed-not-committed"
				current := original
				switch change {
				case "state":
					current.State = RecordLive
				case "generation":
					current.Generation++
				case "metadata":
					current.Handoff = true
				case "removed":
					if err := os.Remove(recordFile(dir, original.ID)); err != nil {
						t.Fatal(err)
					}
				case "malformed":
					if err := os.WriteFile(recordFile(dir, original.ID), []byte("private-snapshot-sentinel"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "wrong-id":
					next.ID = "cb-ffff"
				case "wrong-version":
					original.Version++
					if err := saveRecord(dir, original); err != nil {
						t.Fatal(err)
					}
					original, err = loadRecord(dir, original.ID)
					if err != nil {
						t.Fatal(err)
					}
				case "next-version":
					next.Version++
				}
				if change == "state" || change == "generation" || change == "metadata" {
					if err := saveRecord(dir, current); err != nil {
						t.Fatal(err)
					}
				}
				before, beforeErr := os.ReadFile(recordFile(dir, current.ID))
				var replacement *sessionRecord
				if kind == "replace" {
					replacement = &next
				}
				// A delete has no proposed replacement ID to reject.
				valid := change == "unchanged" || (kind == "delete" && (change == "wrong-id" || change == "next-version"))
				got, err := mutateRecordIfCurrent(dir, original, replacement)
				if browserRegistryGateRefs(t, dir) != 0 {
					t.Fatal("snapshot mutation retained its registry gate")
				}
				if valid {
					if err != nil {
						t.Fatal("current snapshot could not commit")
					}
					if kind == "delete" {
						if _, err := os.Stat(recordFile(dir, original.ID)); !errors.Is(err, os.ErrNotExist) {
							t.Fatal("matching record was not deleted")
						}
					} else {
						saved, err := loadRecord(dir, original.ID)
						if err != nil || !reflect.DeepEqual(saved, got) || saved.State != next.State || saved.Generation != next.Generation || saved.UpdatedAt == "" {
							t.Fatal("replacement did not return its actual persisted snapshot")
						}
					}
					return
				}
				if err == nil || got.ID != "" || strings.Contains(err.Error(), "private-snapshot-sentinel") {
					t.Fatal("invalid snapshot acknowledged a mutation or exposed details")
				}
				after, afterErr := os.ReadFile(recordFile(dir, current.ID))
				if !reflect.DeepEqual(before, after) || errors.Is(beforeErr, os.ErrNotExist) != errors.Is(afterErr, os.ErrNotExist) {
					t.Fatal("refused mutation changed the current record")
				}
				if _, err := os.Stat(recordFile(dir, "cb-ffff")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("replacement wrote a different session")
				}
			})
		}
	}
}

func TestSuspendRegistrySnapshotRecoveryPreservesNewerRecord(t *testing.T) {
	for _, kind := range []string{"host", "container"} {
		t.Run(kind, func(t *testing.T) {
			// Reject before any runtime/process probe. No resource adapter is
			// configured, and the host profile intentionally cannot match.
			adoption, runner, resolver := adoptionSupported, activeBrowserRunner, registryDirResolver
			adoptionSupported, activeBrowserRunner, registryDirResolver = true, nil, nil
			t.Cleanup(func() { adoptionSupported, activeBrowserRunner, registryDirResolver = adoption, runner, resolver })
			m := NewCobrowseSessionManager(t.TempDir(), 4)
			m.useResolver = true
			dir := m.registryDir()
			original := sessionRecord{Version: sessionRecordVersion, ID: "cb-abcd", State: RecordLive, Generation: 4}
			if kind == "container" {
				original.Runner = "container"
			}
			if err := saveRecord(dir, original); err != nil {
				t.Fatal(err)
			}
			original, err := loadRecord(dir, original.ID)
			if err != nil {
				t.Fatal(err)
			}
			release, err := acquireBrowserRegistryGate(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			done := make(chan error, 1)
			var expired *sessionRecord
			go func() {
				if kind == "container" {
					expired = m.recoverContainer(original, filepath.Join(m.baseDir, original.ID), dir, time.Now())
				} else {
					m.recoverSessions()
				}
				done <- nil
			}()
			waiting, consumed := waitBrowserRegistryWriter(t, dir, done)
			next := original
			next.State, next.Generation = RecordSuspended, 5
			if err := saveRecordUnderRegistryGate(dir, next); err != nil {
				t.Fatal(err)
			}
			expected, err := loadRecord(dir, next.ID)
			if err != nil {
				t.Fatal(err)
			}
			release()
			if !consumed {
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("recovery did not finish after exclusion")
				}
			}
			got, err := loadRecord(dir, next.ID)
			if !waiting || err != nil || !reflect.DeepEqual(got, expected) || expired != nil {
				t.Fatal("stale recovery snapshot expired a newer lifecycle record")
			}
			if len(m.sessions) != 0 {
				t.Fatal("failed recovery adopted a session")
			}
		})
	}
}

func TestSuspendRegistrySnapshotRecoveryAndRetentionStillProgress(t *testing.T) {
	adoption, runner, resolver := adoptionSupported, activeBrowserRunner, registryDirResolver
	adoptionSupported, activeBrowserRunner, registryDirResolver = true, nil, nil
	t.Cleanup(func() { adoptionSupported, activeBrowserRunner, registryDirResolver = adoption, runner, resolver })
	m := NewCobrowseSessionManager(t.TempDir(), 4)
	m.useResolver = true
	dir := m.registryDir()
	for i := 0; i < expiredMaxRecords+2; i++ {
		rec := sessionRecord{Version: sessionRecordVersion, ID: fmt.Sprintf("cb-%04x", i), State: RecordLive, Generation: 1}
		if i%2 == 1 {
			rec.Runner = "container"
		}
		if err := saveRecord(dir, rec); err != nil {
			t.Fatal(err)
		}
	}
	m.recoverSessions()
	all := listRecords(dir)
	if len(all) != expiredMaxRecords {
		t.Fatal("newly expired snapshots did not participate in retention pruning")
	}
	for _, rec := range all {
		if rec.State != RecordExpired || rec.EndedReason != endedReasonLost || rec.UpdatedAt == "" {
			t.Fatal("matching recovery snapshot did not expire normally")
		}
	}
	// Age pruning must also still work on an unchanged snapshot.
	for _, rec := range all {
		rec.EndedAt = time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
		if err := saveRecord(dir, rec); err != nil {
			t.Fatal(err)
		}
	}
	m.recoverSessions()
	if len(listRecords(dir)) != 0 {
		t.Fatal("matching age-prune snapshots were retained")
	}
}
