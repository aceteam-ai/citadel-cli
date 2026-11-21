package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// All runtime operations are fake; only private test-owned files are removed.
func suspendDeleteFixture(t *testing.T) (*CobrowseSessionManager, *cobrowseSession, *suspendIntentStore, sessionRecord, preparedSuspendCapture, *int) {
	t.Helper()
	s, _ := suspendWriterFixture(t)
	s.ownerUserID, s.proc.debugPort = "owner-a", 9876
	s.proc.exited = make(chan struct{})
	stops := new(int)
	s.proc.stop = func() error { *stops++; return nil }
	s.persistRecord()
	live, err := loadRecord(s.regDir, s.id)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openSuspendIntentStore(s.regDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	manifest := suspendTestManifest()
	manifest.SessionID, manifest.Generation = live.ID, live.Generation
	capture, err := prepareSuspendCapture(manifest, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(capture.clear)
	m := newCobrowseSessionManager(filepath.Dir(s.profile), 4)
	m.sessions[s.id] = s
	return m, s, store, live, capture, stops
}

func assertSuspendDeleteRetained(t *testing.T, store *suspendIntentStore, rec sessionRecord, before []byte) {
	t.Helper()
	after, err := os.ReadFile(recordFile(store.root.Name(), rec.ID))
	if err != nil || string(after) != string(before) {
		t.Fatal("ordinary Stop deleted or changed retained lifecycle metadata")
	}
	manifest, err := store.payloads.read(rec, nil)
	if err != nil || len(manifest.Tabs) != 2 {
		t.Fatal("ordinary Stop lost a referenced private capture")
	}
}

func TestSuspendDeletePublicStopRetainsLifecycle(t *testing.T) {
	for _, kind := range []string{"suspending", "suspended", "resuming", "live-retained", "expired-retained", "tombstone", "uncertain-publish", "resource-error"} {
		t.Run(kind, func(t *testing.T) {
			m, s, store, live, capture, stops := suspendDeleteFixture(t)
			flush := store.syncDir
			if kind == "uncertain-publish" {
				store.syncDir = func() error { return errors.New("private-delete-sentinel") }
			}
			intent, err := store.stage(suspendPagesContext(t), live, live.OwnerUserID, capture, suspendTestNow, func() error { return nil })
			store.syncDir = flush
			if kind == "uncertain-publish" {
				if err == nil {
					t.Fatal("fixture did not produce an uncertain commit")
				}
				intent, err = store.read(live.ID)
			}
			if err != nil || intent.State != "suspending" {
				t.Fatal("fixture did not retain a published capture intent", err)
			}
			next := intent
			switch kind {
			case "suspended":
				next.State = RecordSuspended
			case "resuming":
				next.State, next.Generation = "resuming", next.Generation+1
			case "live-retained":
				next.State = RecordLive
			case "expired-retained":
				next.State = RecordExpired
			case "tombstone":
				next.State, next.Suspension, next.EndedReason = RecordExpired, nil, "suspend_expired"
			}
			if kind != "suspending" && kind != "uncertain-publish" && kind != "resource-error" {
				if err := saveRecord(s.regDir, next); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(recordFile(s.regDir, s.id))
			if err != nil {
				t.Fatal(err)
			}
			proofs := 0
			s.proc.stopForSuspend = func(context.Context) error { proofs++; return nil }
			resourceErr := errors.New("synthetic resource cleanup failure")
			if kind == "resource-error" {
				s.proc.stop = func() error { *stops++; return resourceErr }
			}
			stopErr := m.Stop(s.id)
			if stopErr == nil || !strings.Contains(stopErr.Error(), "metadata retained") ||
				strings.Contains(stopErr.Error(), "private-delete-sentinel") || *stops != 1 || proofs != 0 {
				t.Fatal("Stop silently discarded lifecycle data, skipped legacy cleanup or claimed positive proof", stopErr)
			}
			if kind == "resource-error" && !errors.Is(stopErr, resourceErr) {
				t.Fatal("metadata refusal hid the resource cleanup error")
			}
			assertSuspendDeleteRetained(t, store, intent, before)
			if len(m.sessions) != 0 || s.proc != nil {
				t.Fatal("ordinary Stop failed to retire its legacy in-memory slot")
			}
			if _, err := os.Stat(s.profile); !os.IsNotExist(err) {
				t.Fatal("ordinary Stop no longer cleaned its private working directory")
			}
		})
	}
}

func TestSuspendDeleteStopAllStillCleansOrdinarySessions(t *testing.T) {
	m, s, store, live, capture, stops := suspendDeleteFixture(t)
	intent, err := store.stage(suspendPagesContext(t), live, live.OwnerUserID, capture, suspendTestNow, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(recordFile(s.regDir, s.id))
	if err != nil {
		t.Fatal(err)
	}
	otherID := "cb-0000abcd"
	otherDir := filepath.Join(m.baseDir, otherID)
	if err := os.Mkdir(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	otherStops := 0
	m.sessions[otherID] = &cobrowseSession{id: otherID, state: SessionRunning, profile: otherDir, regDir: s.regDir,
		proc: &cobrowseProc{stop: func() error { otherStops++; return nil }}}
	if err := saveRecord(s.regDir, sessionRecord{Version: sessionRecordVersion, ID: otherID, State: RecordLive}); err != nil {
		t.Fatal(err)
	}
	if err := m.StopAll(); err == nil || *stops != 1 || otherStops != 1 || len(m.sessions) != 0 {
		t.Fatal("one retained record prevented StopAll from cleaning its other sessions", err)
	}
	assertSuspendDeleteRetained(t, store, intent, before)
	if _, err := os.Stat(recordFile(s.regDir, otherID)); !os.IsNotExist(err) {
		t.Fatal("ordinary sibling record was not deleted")
	}
	if _, err := os.Stat(otherDir); !os.IsNotExist(err) {
		t.Fatal("ordinary sibling working directory was not removed")
	}
}

func TestSuspendDeleteRechecksAfterPublicationDuringWait(t *testing.T) {
	m, s, store, live, capture, stops := suspendDeleteFixture(t)
	done := make(chan error, 1)
	var intent sessionRecord
	var before []byte
	err := store.withLifecycleExclusive(suspendPagesContext(t), func(context.Context) error {
		go func() { done <- m.Stop(s.id) }()
		waiting, consumed := waitBrowserRegistryWriter(t, s.regDir, done)
		if !waiting || consumed {
			return errors.New("Stop did not wait for exclusion")
		}
		var err error
		intent, err = store.stage(suspendPagesContext(t), live, live.OwnerUserID, capture, suspendTestNow, func() error { return nil })
		if err != nil {
			return err
		}
		before, err = os.ReadFile(recordFile(s.regDir, s.id))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || *stops != 1 {
			t.Fatal("waiting Stop erased a newly published intent or skipped resource cleanup", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not finish after registry release")
	}
	assertSuspendDeleteRetained(t, store, intent, before)
}

func TestSuspendDeleteUnknownMetadataIsNotDiscardAuthority(t *testing.T) {
	for _, kind := range []string{"unknown-state", "version", "wrong-id", "invalid-json", "decode-error", "oversized", "empty", "directory", "unsafe-file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			_, dir := suspendStartFixture(t)
			rec := sessionRecord{Version: sessionRecordVersion, ID: suspendStartTestID, State: RecordLive}
			switch kind {
			case "unknown-state":
				rec.State = "future"
			case "version":
				rec.Version++
			case "wrong-id":
				rec.ID = "cb-abcd"
			}
			before, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "invalid-json":
				before = []byte("private-delete-sentinel invalid")
			case "decode-error":
				before = []byte(`{"version":2,"id":"` + suspendStartTestID + `","state":"live","generation":"private-delete-sentinel"}`)
			case "oversized":
				before = []byte(`{"version":2,"id":"` + suspendStartTestID + `","state":"live","padding":"` + strings.Repeat("x", maxSuspendRegistryRecordBytes) + `"}`)
			case "empty":
				before = nil
			}
			name := recordFile(dir, suspendStartTestID)
			if kind == "directory" {
				if err := os.Mkdir(name, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if kind == "symlink" {
				target := filepath.Join(dir, "private-target")
				if err := os.WriteFile(target, before, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, name); err != nil {
					t.Skipf("synthetic symlink unavailable: %v", err)
				}
			} else {
				if err := os.WriteFile(name, before, 0o600); err != nil {
					t.Fatal(err)
				}
				if kind == "unsafe-file" {
					if runtime.GOOS == "windows" {
						t.Skip("Unix permission refusal; Windows registry root uses a private DACL")
					}
					if err := os.Chmod(name, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := deleteLegacyBrowserRecord(suspendPagesContext(t), dir, suspendStartTestID); err == nil || strings.Contains(err.Error(), "private-delete-sentinel") {
				t.Fatal("unknown metadata granted discard or leaked private content", err)
			}
			if _, err := os.Lstat(name); err != nil {
				t.Fatal("refused deletion changed the unknown entry")
			}
			if kind != "directory" {
				after, err := os.ReadFile(name)
				if err != nil || string(after) != string(before) {
					t.Fatal("refused deletion changed private bytes")
				}
			}
		})
	}
}

func TestSuspendDeleteLegacyCompatibilityAndAdmission(t *testing.T) {
	for _, state := range []string{RecordLive, RecordExpired, "missing"} {
		t.Run(state, func(t *testing.T) {
			_, dir := suspendStartFixture(t)
			if state != "missing" {
				writeSuspendStartRecord(t, dir, sessionRecord{Version: sessionRecordVersion, ID: suspendStartTestID, State: state})
			}
			if err := deleteRecord(dir, suspendStartTestID); err != nil {
				t.Fatal("legacy record could no longer be stopped", err)
			}
			if _, err := os.Stat(recordFile(dir, suspendStartTestID)); !os.IsNotExist(err) {
				t.Fatal("ordinary record was not removed")
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "not-created")
	if err := deleteRecord(missing, suspendStartTestID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("deletion created missing storage")
	}
	for _, ctx := range []context.Context{nil, context.Background()} {
		if deleteLegacyBrowserRecord(ctx, missing, suspendStartTestID) == nil {
			t.Fatal("unbounded/nil deletion was admitted")
		}
	}
	ctx, cancel := context.WithCancel(suspendPagesContext(t))
	cancel()
	if deleteLegacyBrowserRecord(ctx, missing, suspendStartTestID) == nil ||
		deleteLegacyBrowserRecord(suspendPagesContext(t), missing, "../bad") == nil {
		t.Fatal("cancelled or malformed deletion was admitted")
	}
	// With no retained metadata refusal, preserve the old resource-error
	// identity rather than wrapping it just because registry cleanup ran.
	m, s, _, _, _, _ := suspendDeleteFixture(t)
	resourceErr := errors.New("synthetic ordinary resource failure")
	s.proc.stop = func() error { return resourceErr }
	if err := m.Stop(s.id); err != resourceErr {
		t.Fatal("ordinary Stop resource-error identity changed", err)
	}
}

func TestSuspendDeleteLongWaitIsCapped(t *testing.T) {
	_, dir := suspendStartFixture(t)
	writeSuspendStartRecord(t, dir, sessionRecord{Version: sessionRecordVersion, ID: suspendStartTestID, State: RecordLive})
	release, err := acquireBrowserRegistryGate(suspendPagesContext(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	start := time.Now()
	if err := deleteLegacyBrowserRecord(ctx, dir, suspendStartTestID); err == nil {
		t.Fatal("deletion bypassed active exclusion")
	}
	if elapsed := time.Since(start); elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Fatal("long parent did not retain the five-second exclusion ceiling", elapsed)
	}
	if _, err := os.Stat(recordFile(dir, suspendStartTestID)); err != nil {
		t.Fatal("timed-out deletion changed metadata")
	}
}

func TestSuspendDeleteWaitBudgetAndRootReplacement(t *testing.T) {
	_, dir := suspendStartFixture(t)
	before := writeSuspendStartRecord(t, dir, sessionRecord{Version: sessionRecordVersion, ID: suspendStartTestID, State: RecordLive})
	release, err := acquireBrowserRegistryGate(suspendPagesContext(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := deleteLegacyBrowserRecord(ctx, dir, suspendStartTestID); err == nil {
		t.Fatal("deletion ignored the exclusion-wait deadline")
	}
	done := make(chan error, 1)
	go func() { done <- deleteLegacyBrowserRecord(suspendPagesContext(t), dir, suspendStartTestID) }()
	waiting, consumed := waitBrowserRegistryWriter(t, dir, done)
	if !waiting || consumed {
		t.Fatal("deletion did not wait for registry exclusion")
	}
	old := dir + ".old"
	if err := os.Rename(dir, old); err != nil {
		t.Fatal(err)
	}
	if err := ensureCobrowseBaseDir(dir); err != nil {
		t.Fatal(err)
	}
	writeSuspendStartRecord(t, dir, sessionRecord{Version: sessionRecordVersion, ID: suspendStartTestID, State: RecordLive})
	newBefore, err := os.ReadFile(recordFile(dir, suspendStartTestID))
	if err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("deletion accepted a replaced registry root")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replaced-root deletion did not finish")
	}
	for path, want := range map[string][]byte{old: before, dir: newBefore} {
		got, err := os.ReadFile(recordFile(path, suspendStartTestID))
		if err != nil || string(got) != string(want) {
			t.Fatal("replaced-root refusal modified a directory")
		}
	}
}
