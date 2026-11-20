package platform

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSuspendDormantStatusLifecycleProjection(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		rec, _ := captureRecord(t, suspendTestManifest(), persistent, &fakeSuspendSealer{})
		expiry, _ := suspendExpiry(rec)
		for _, state := range []string{RecordSuspended, "suspending", "resuming", RecordExpired} {
			t.Run(state+map[bool]string{false: "-throwaway", true: "-persistent"}[persistent], func(t *testing.T) {
				current := rec
				now := suspendTestNow.Add(time.Hour)
				if state == RecordExpired {
					current, _, _ = planSuspendExpiry(current, expiry)
					current.UpdatedAt = expiry.Format(time.RFC3339Nano)
					now = expiry
				} else {
					current.State = state
					if state == "resuming" {
						current.Generation++
					}
				}
				got, ok := dormantSessionStatus(current, now)
				if !ok || got.State != CobrowseSessionState(state) || got.ID != current.ID ||
					got.Generation != current.Generation || got.ExpiresAt != rec.Suspension.ExpiresAt {
					t.Fatal("dormant lifecycle metadata was lost", got)
				}
				kind := "throwaway"
				if persistent {
					kind = "persistent"
				}
				if got.ProfileKind != kind || (state != RecordExpired && got.TabCount != rec.Suspension.TabCount) {
					t.Fatal("profile kind or retained count changed", got)
				}
				encoded, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"url", "title", "profile", "debug_port", "display", "driver", "targets", "owner_user_id", "page_text"} {
					if strings.Contains(string(encoded), `"`+key+`":`) {
						t.Fatal("dormant status acquired live/private content", key)
					}
				}
				if state == RecordExpired && (got.TabCount != 0 || got.EndedReason != "suspend_expired") {
					t.Fatal("expired status lost its reason or retained tabs")
				}
			})
		}
	}
}

func TestSuspendDormantStatusExpiryIsReadOnly(t *testing.T) {
	rec, _ := captureRecord(t, suspendTestManifest(), false, nil)
	expiry, _ := suspendExpiry(rec)
	before, _ := json.Marshal(rec)
	got, ok := dormantSessionStatus(rec, expiry)
	if !ok || got.State != SessionExpired || got.TabCount != 0 || got.EndedReason != "suspend_expired" {
		t.Fatal("elapsed captures remain available", got)
	}
	after, _ := json.Marshal(rec)
	if string(before) != string(after) || rec.State != RecordSuspended || rec.Suspension.TabCount == 0 {
		t.Fatal("status performed an expiry transition")
	}
	if _, ok := dormantSessionStatus(rec, expiry.Add(suspendTombstoneTTL)); ok {
		t.Fatal("expired metadata outlived tombstone retention")
	}
}

func TestSuspendDormantStatusRejectsInvalidMetadata(t *testing.T) {
	for _, kind := range []string{"live", "unknown", "nil", "generation", "ttl", "version", "expired-receipt", "live-process", "zero-clock", "early-clock"} {
		t.Run(kind, func(t *testing.T) {
			rec, _ := captureRecord(t, suspendTestManifest(), false, nil)
			now := suspendTestNow.Add(time.Hour)
			switch kind {
			case "live":
				rec.State = RecordLive
			case "unknown":
				rec.State = "pending_unlock"
			case "nil":
				rec.Suspension = nil
			case "generation":
				rec.Generation++
			case "ttl":
				rec.Suspension.ExpiresAt = now.Format(time.RFC3339Nano)
			case "version":
				rec.Version++
			case "expired-receipt":
				rec.State = RecordExpired
			case "live-process":
				rec.BrowserPID = 42
			case "zero-clock":
				now = time.Time{}
			case "early-clock":
				now = suspendTestNow.Add(-time.Nanosecond)
			}
			if _, ok := dormantSessionStatus(rec, now); ok {
				t.Fatal("invalid lifecycle was exposed", kind)
			}
		})
	}
}

func TestSuspendDormantStatusManagerRestartAndLocalIsolation(t *testing.T) {
	base := trustedBaseDir(t)
	m := newCobrowseSessionManager(base, 2)
	m.useResolver = true
	m.sweepOnce.Do(func() {}) // No real adoption/sweep is part of this read test.
	rec, _ := captureRecord(t, suspendTestManifest(), false, nil)
	now := time.Now().UTC()
	rec.Suspension.SuspendedAt = now.Add(-time.Minute).Format(time.RFC3339Nano)
	rec.Suspension.ExpiresAt = now.Add(-time.Minute).Add(throwawaySuspendTTL).Format(time.RFC3339Nano)
	if err := saveRecord(m.registryDir(), rec); err != nil {
		t.Fatal(err)
	}
	path := recordFile(m.registryDir(), rec.ID)
	before, _ := os.ReadFile(path)
	// No payload directory exists: status must not create/open a store or require a key.
	for i := 0; i < 2; i++ {
		manager := newCobrowseSessionManager(base, 2)
		manager.useResolver = true
		manager.sweepOnce.Do(func() {})
		status, ok := manager.SessionStatus(rec.ID)
		if !ok || status.State != SessionSuspended || status.TabCount != rec.Suspension.TabCount || status.Profile != "" {
			t.Fatal("restart forgot dormant session or probed a runtime", status)
		}
		if all := manager.List(); len(all) != 1 || !reflect.DeepEqual(all[0], status) {
			t.Fatal("list and lookup disagree", all)
		}
		if _, ok := manager.SessionStatus(""); ok {
			t.Fatal("empty id selected a dormant session")
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("status rewrote durable metadata")
	}
	if _, err := os.Stat(filepath.Join(m.registryDir(), suspendManifestDirName)); !os.IsNotExist(err) {
		t.Fatal("status opened a payload store", err)
	}
	local := newCobrowseSessionManager(base, 2)
	local.sweepOnce.Do(func() {})
	if _, ok := local.SessionStatus(rec.ID); ok || len(local.List()) != 0 {
		t.Fatal("local manager inherited node registry")
	}
	// Existing in-memory state wins rather than duplicating/replacing it with disk.
	m.sessions[rec.ID] = &cobrowseSession{id: rec.ID, state: SessionLaunching, generation: rec.Generation + 1}
	if got, ok := m.SessionStatus(rec.ID); !ok || got.State != SessionLaunching {
		t.Fatal("dormant snapshot replaced live memory")
	}
	if all := m.List(); len(all) != 1 || all[0].State != SessionLaunching {
		t.Fatal("session appeared twice", all)
	}
}

func TestSuspendDormantStatusReadBoundsAndMissingRegistry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if all := readDormantSessionStatuses(dir, "", suspendTestNow); len(all) != 0 {
		t.Fatal(all)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("read created a registry")
	}
	if err := ensureCobrowseBaseDir(dir); err != nil {
		t.Fatal(err)
	}
	rec, _ := captureRecord(t, suspendTestManifest(), false, nil)
	if err := saveRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../cb-aa", "cb-", "cb-" + strings.Repeat("a", 65)} {
		if all := readDormantSessionStatuses(dir, id, suspendTestNow); len(all) != 0 {
			t.Fatal("invalid lookup", all)
		}
	}
	if err := os.WriteFile(recordFile(dir, rec.ID), []byte(strings.Repeat("x", maxSuspendRegistryRecordBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if all := readDormantSessionStatuses(dir, rec.ID, suspendTestNow); len(all) != 0 {
		t.Fatal("oversize record exposed", all)
	}
}

func TestSuspendDormantStatusListResourceBounds(t *testing.T) {
	for _, bound := range []string{"entries", "bytes"} {
		t.Run(bound, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "registry")
			rec, _ := captureRecord(t, suspendTestManifest(), false, nil)
			if err := saveRecord(dir, rec); err != nil {
				t.Fatal(err)
			}
			if bound == "entries" {
				for i := 0; i < maxSuspendRegistryEntries; i++ {
					rec.ID = fmt.Sprintf("cb-%04x", i)
					data, err := json.Marshal(rec)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(recordFile(dir, rec.ID), data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				for i := 0; i <= maxSuspendRegistryBytes/(maxSuspendRegistryRecordBytes-1024); i++ {
					rec.ID = fmt.Sprintf("cb-%04x", i)
					data, err := json.Marshal(rec)
					if err != nil {
						t.Fatal(err)
					}
					padding := strings.Repeat("x", maxSuspendRegistryRecordBytes-len(data)-32)
					data = append(data[:len(data)-1], []byte(`,"ignored":"`+padding+`"}`)...)
					if err := os.WriteFile(recordFile(dir, rec.ID), data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if all := readDormantSessionStatuses(dir, "", suspendTestNow); len(all) != 0 {
				t.Fatal("list exceeded resource bound and reported a partial inventory", bound, len(all))
			}
			if all := readDormantSessionStatuses(dir, rec.ID, suspendTestNow); len(all) != 1 {
				t.Fatal("bounded single-ID lookup incorrectly scanned siblings", bound, len(all))
			}
		})
	}
}
