package platform

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// Recovery receives only private synthetic records. The process/CDP table is
// fake, and there are no live sessions, browser launches, or profile handles.
func suspendRecoveryManager(t *testing.T) (*CobrowseSessionManager, string) {
	t.Helper()
	newAdoptEnv(t)
	base := trustedBaseDir(t)
	m := newCobrowseSessionManager(base, 4)
	reg := m.registryDir()
	previous := registryDirResolver
	registryDirResolver = func() string { return reg }
	t.Cleanup(func() { registryDirResolver = previous })
	m.useResolver = true
	return m, reg
}

func TestSuspendRecoveryLeavesTombstonesToDurableSweep(t *testing.T) {
	m, reg := suspendRecoveryManager(t)
	now := time.Now().UTC()
	want := make(map[string][]byte)
	for i, age := range []time.Duration{2 * 24 * time.Hour, 8 * 24 * time.Hour, time.Hour} {
		rec := sessionRecord{Version: sessionRecordVersion, ID: fmt.Sprintf("cb-aa%d", i),
			State: RecordExpired, EndedReason: "suspend_expired", EndedAt: now.Add(-age).Format(time.RFC3339Nano)}
		if err := saveRecord(reg, rec); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(recordFile(reg, rec.ID))
		if err != nil {
			t.Fatal(err)
		}
		want[rec.ID] = data
	}
	// All legacy records are newer than the one-hour suspend tombstone, so
	// including it in the legacy cap would evict it despite its seven-day TTL.
	for i := range expiredMaxRecords + 5 {
		rec := sessionRecord{Version: sessionRecordVersion, ID: fmt.Sprintf("cb-bb%x", i),
			State: RecordExpired, EndedReason: "node_update", EndedAt: now.Add(-time.Duration(i) * time.Second).Format(time.RFC3339Nano)}
		if err := saveRecord(reg, rec); err != nil {
			t.Fatal(err)
		}
	}
	old := sessionRecord{Version: sessionRecordVersion, ID: "cb-cccc", State: RecordExpired,
		EndedReason: "node_update", EndedAt: now.Add(-25 * time.Hour).Format(time.RFC3339Nano)}
	if err := saveRecord(reg, old); err != nil {
		t.Fatal(err)
	}
	m.recoverSessions()
	for id, data := range want {
		got, err := os.ReadFile(recordFile(reg, id))
		if err != nil || string(got) != string(data) {
			t.Error("legacy recovery deleted or changed a TTL-owned tombstone")
		}
	}
	if _, err := os.Stat(recordFile(reg, old.ID)); !os.IsNotExist(err) {
		t.Error("legacy non-suspend retention changed")
	}
	count := 0
	for _, rec := range listRecords(reg) {
		if rec.EndedReason == "node_update" {
			count++
		}
	}
	if count != expiredMaxRecords {
		t.Error("legacy non-suspend cap changed")
	}
}

func TestSuspendExpiredLookupKeepsSevenDayReason(t *testing.T) {
	m, reg := suspendRecoveryManager(t)
	for _, tc := range []struct {
		id, reason string
		age        time.Duration
		want       bool
	}{
		{"cb-d1", "suspend_expired", 2 * 24 * time.Hour, true},
		{"cb-d2", "suspend_expired", 8 * 24 * time.Hour, false},
		{"cb-d3", "node_update", 2 * 24 * time.Hour, false},
		{"cb-d4", "node_update", time.Hour, true},
	} {
		t.Run(tc.id, func(t *testing.T) {
			rec := sessionRecord{Version: sessionRecordVersion, ID: tc.id, State: RecordExpired,
				EndedReason: tc.reason, EndedAt: time.Now().Add(-tc.age).UTC().Format(time.RFC3339Nano)}
			if err := saveRecord(reg, rec); err != nil {
				t.Fatal(err)
			}
			ended, ok := m.EndedSession(rec.ID)
			if ok != tc.want || (ok && (ended.Reason != tc.reason || ended.At != rec.EndedAt)) {
				t.Error("ended lookup lost the correct reason or retention window")
			}
		})
	}
}
