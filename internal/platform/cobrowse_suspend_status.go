package platform

// Dormant status is a read-only projection through the existing manager status
// boundary. It confers no suspend/reopen authority, opens no manifest/profile,
// and never connects to a browser or changes a lifecycle record.

import (
	"encoding/json"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

func dormantSessionStatus(rec sessionRecord, now time.Time) (CobrowseSessionStatus, bool) {
	if now.IsZero() {
		return CobrowseSessionStatus{}, false
	}
	var expiry time.Time
	var err error
	switch rec.State {
	case RecordSuspended, "suspending", "resuming":
		expiry, err = suspendExpiry(rec)
	case RecordExpired:
		if _, err = suspendTombstoneCaptureName(rec); err == nil {
			expiry, err = suspendCaptureExpiry(rec)
		}
	default:
		return CobrowseSessionStatus{}, false
	}
	if err != nil || now.Before(mustSuspendCapturedAt(rec)) {
		return CobrowseSessionStatus{}, false
	}
	if rec.State == RecordSuspended || rec.State == RecordExpired {
		identity := suspendIdentity(rec)
		identity.SessionID, identity.Generation = "", 0
		if identity != (suspendStopIdentity{}) || rec.Profile != "" || rec.Display != "" ||
			rec.Browser != "" || rec.OwnerPID != 0 || rec.OwnerStart != 0 || rec.OwnerUID != nil {
			return CobrowseSessionStatus{}, false
		}
	}
	st := CobrowseSessionStatus{ID: rec.ID, State: CobrowseSessionState(rec.State), Generation: rec.Generation,
		ExpiresAt: expiry.UTC().Format(time.RFC3339Nano), TabCount: rec.Suspension.TabCount, ProfileKind: "throwaway"}
	if rec.Persistent {
		st.ProfileKind = "persistent"
	}
	if rec.State == RecordExpired || (rec.State == RecordSuspended && !now.Before(expiry)) {
		if !now.Before(expiry.Add(suspendTombstoneTTL)) {
			return CobrowseSessionStatus{}, false
		}
		st.State, st.TabCount, st.EndedReason = SessionExpired, 0, "suspend_expired"
	}
	return st, true
}

// Only called after suspendExpiry/suspendCaptureExpiry validated this timestamp.
func mustSuspendCapturedAt(rec sessionRecord) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, rec.Suspension.SuspendedAt)
	return at
}

func (m *CobrowseSessionManager) dormantSessionStatuses(id string) []CobrowseSessionStatus {
	// Local mode has no durable registry and must never inherit a sibling's.
	if !m.useResolver {
		return nil
	}
	return readDormantSessionStatuses(m.registryDir(), id, time.Now())
}

func readDormantSessionStatuses(dir, id string, now time.Time) []CobrowseSessionStatus {
	if id != "" && (!sessionIDPattern.MatchString(id) || len(id) > 64) {
		return nil
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || validateCobrowseBaseDirPerms(dir, info) != nil ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil
	}
	defer root.Close()
	unchanged := func() bool {
		f, err := root.Open(".")
		if err != nil {
			return false
		}
		defer f.Close()
		opened, e1 := f.Stat()
		named, e2 := os.Lstat(dir)
		return e1 == nil && e2 == nil && os.SameFile(info, opened) && os.SameFile(info, named) &&
			validateCobrowseBaseDirPerms(dir, opened) == nil &&
			(runtime.GOOS == "windows" || opened.Mode().Perm()&0o077 == 0)
	}
	if !unchanged() {
		return nil
	}
	ids := []string{id}
	if id == "" {
		f, err := root.Open(".")
		if err != nil {
			return nil
		}
		defer f.Close()
		entries, err := f.ReadDir(maxSuspendRegistryEntries)
		if (err != nil && err != io.EOF) || len(entries) >= maxSuspendRegistryEntries {
			return nil
		}
		ids = nil
		for _, entry := range entries {
			candidate := strings.TrimSuffix(entry.Name(), ".json")
			if candidate != entry.Name() && len(candidate) <= 64 && sessionIDPattern.MatchString(candidate) {
				ids = append(ids, candidate)
			}
		}
		sort.Strings(ids)
	}
	var out []CobrowseSessionStatus
	var used int64
	for _, candidate := range ids {
		name := candidate + ".json"
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxSuspendRegistryRecordBytes ||
			(runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
			continue
		}
		used += info.Size()
		if used > maxSuspendRegistryBytes {
			return nil
		}
		f, err := root.Open(name)
		if err != nil {
			continue
		}
		opened, statErr := f.Stat()
		if statErr != nil || !os.SameFile(info, opened) {
			_ = f.Close()
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(f, maxSuspendRegistryRecordBytes+1))
		_ = f.Close()
		var rec sessionRecord
		if readErr != nil || len(data) > maxSuspendRegistryRecordBytes || json.Unmarshal(data, &rec) != nil || rec.ID != candidate {
			continue
		}
		if status, ok := dormantSessionStatus(rec, now); ok {
			out = append(out, status)
		}
	}
	if !unchanged() {
		return nil
	}
	return out
}
