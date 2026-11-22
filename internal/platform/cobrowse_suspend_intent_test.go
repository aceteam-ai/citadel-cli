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
	"sync"
	"testing"
	"time"
)

func suspendIntentFixture(t *testing.T) (*suspendIntentStore, string, sessionRecord, preparedSuspendCapture) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "registry")
	s, err := openSuspendIntentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.close() })
	live := suspendTestLive()
	live.BrowserID, live.BrowserPID, live.DebugPort = "browser-a", 12345, 9876
	live.BrowserScope, live.XvfbScope = "browser-synthetic.scope", "display-synthetic.scope"
	live.Runner, live.ContainerID, live.ContainerStart, live.ControlPort = "container", "synthetic-container", "synthetic-start", 9001
	if err := saveRecord(dir, live); err != nil {
		t.Fatal(err)
	}
	live, err = s.read(live.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := suspendTestManifest()
	m.SessionID, m.Generation = live.ID, live.Generation
	p, err := prepareSuspendCapture(m, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.clear)
	return s, dir, live, p
}

func TestSuspendIntentPayloadBeforeDurableIntent(t *testing.T) {
	s, dir, live, capture := suspendIntentFixture(t)
	var order []string
	payloadSync, fileSync, rename, dirSync := s.payloads.syncDir, s.syncFile, s.rename, s.syncDir
	s.payloads.syncDir = func() error { order = append(order, "payload"); return payloadSync() }
	s.syncFile = func(f *os.File) error { order = append(order, "file"); return fileSync(f) }
	s.rename = func(a, b string) error { order = append(order, "rename"); return rename(a, b) }
	s.syncDir = func() error { order = append(order, "directory"); return dirSync() }
	intent, err := s.stage(suspendPagesContext(t), live, live.OwnerUserID, capture, suspendTestNow, func() error { return nil })
	if err != nil || !reflect.DeepEqual(order, []string{"payload", "file", "rename", "directory"}) {
		t.Fatal("suspend intent acknowledged before both payload and registry durability")
	}
	if intent.State != "suspending" || intent.BrowserID != live.BrowserID || intent.BrowserPID != live.BrowserPID ||
		intent.BrowserScope != live.BrowserScope || intent.XvfbScope != live.XvfbScope || intent.ContainerID != live.ContainerID ||
		intent.ContainerStart != live.ContainerStart || intent.ControlPort != live.ControlPort || intent.Generation != live.Generation {
		t.Fatal("intent lost the original browser identity needed for crash recovery")
	}
	data, err := os.ReadFile(recordFile(dir, live.ID))
	if err != nil || strings.Contains(string(data), "private-sentinel") || json.Unmarshal(data, &intent) != nil {
		t.Fatal("lifecycle registry contains private page data")
	}
	m, err := s.payloads.read(intent, nil)
	if err != nil || len(m.Tabs) != capture.tabs || m.Generation != live.Generation {
		t.Fatal("acknowledged intent does not point to its complete capture")
	}
	_ = s.close()
	reopened, err := openSuspendIntentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	recovered, err := reopened.read(live.ID)
	if err != nil || recovered.State != "suspending" {
		t.Fatal("intent did not survive an independent reopen")
	}
	if _, err := reopened.payloads.read(recovered, nil); err != nil {
		t.Fatal("payload did not survive an independent reopen")
	}
}

func TestSuspendIntentAdmissionAndFinalAuthority(t *testing.T) {
	for _, kind := range []string{"owner", "legacy", "generation", "record-before", "payload-generation", "deadline", "expired", "nil-fence", "fence-before", "fence-after-payload", "fence-after-sync", "record-after-sync"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, live, capture := suspendIntentFixture(t)
			ctx, owner, fence := suspendPagesContext(t), live.OwnerUserID, func() error { return nil }
			expected := live
			switch kind {
			case "owner":
				owner = "foreign-owner"
			case "legacy":
				expected.OwnerUserID = ""
			case "generation":
				expected.Generation++
			case "record-before":
				expected.BrowserID = "replacement-browser"
			case "payload-generation":
				capture.generation++
			case "deadline":
				ctx = context.Background()
			case "expired":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "nil-fence":
				fence = nil
			case "fence-before":
				fence = func() error { return errors.New("private-sentinel") }
			case "fence-after-payload":
				s.payloads.syncDir = func() error { fence = func() error { return errors.New("private-sentinel") }; return nil }
			case "fence-after-sync":
				s.syncFile = func(*os.File) error { fence = func() error { return errors.New("private-sentinel") }; return nil }
			case "record-after-sync":
				s.syncFile = func(*os.File) error {
					changed := live
					changed.OwnerUserID = "new-owner"
					return saveRecord(dir, changed)
				}
			}
			// Indirect wrapper intentionally observes authority changes made by
			// the fake providers, just as a session fence reads current state.
			check := func() error {
				if fence == nil {
					return errors.New("no authority")
				}
				return fence()
			}
			if kind == "nil-fence" {
				check = nil
			}
			out, err := s.stage(ctx, expected, owner, capture, suspendTestNow, check)
			if err == nil || out.ID != "" || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("stale or foreign authority acknowledged a suspend intent")
			}
			current, err := s.read(live.ID)
			if err != nil || current.State != RecordLive || (kind == "record-after-sync" && current.OwnerUserID != "new-owner") {
				t.Fatal("refused intent replaced the live authority")
			}
			if kind == "record-before" {
				used, err := s.payloads.usage()
				if err != nil || used != 0 {
					t.Fatal("stale initial record consumed manifest capacity")
				}
			}
		})
	}
}

func TestSuspendIntentPersistenceFailuresRetainAccounting(t *testing.T) {
	for _, kind := range []string{"payload", "file", "rename", "directory"} {
		t.Run(kind, func(t *testing.T) {
			s, _, live, capture := suspendIntentFixture(t)
			refusal := errors.New("private-sentinel")
			switch kind {
			case "payload":
				s.payloads.syncFile = func(*os.File) error { return refusal }
			case "file":
				s.syncFile = func(*os.File) error { return refusal }
			case "rename":
				s.rename = func(string, string) error { return refusal }
			case "directory":
				s.syncDir = func() error { return refusal }
			}
			out, err := s.stage(suspendPagesContext(t), live, live.OwnerUserID, capture, suspendTestNow, func() error { return nil })
			if err == nil || out.ID != "" || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("uncertain persistence acknowledged success or leaked private data")
			}
			current, err := s.read(live.ID)
			if err != nil || (kind == "directory" && current.State != "suspending") || (kind != "directory" && current.State != RecordLive) {
				t.Fatal("incorrect crash-visible registry state")
			}
			used, err := s.payloads.usage()
			if err != nil || (kind != "payload" && used < int64(len(capture.data))) {
				t.Fatal("uncertain capture stopped counting against capacity")
			}
		})
	}
}

func TestSuspendIntentAuthorityLostAfterPublicationIsUnacknowledged(t *testing.T) {
	s, _, live, capture := suspendIntentFixture(t)
	ctx, cancel := context.WithCancel(suspendPagesContext(t))
	defer cancel()
	syncDir := s.syncDir
	s.syncDir = func() error { err := syncDir(); cancel(); return err }
	if out, err := s.stage(ctx, live, live.OwnerUserID, capture, suspendTestNow, func() error { return nil }); err == nil || out.ID != "" {
		t.Fatal("cancelled publication was acknowledged")
	}
	current, err := s.read(live.ID)
	if err != nil || current.State != "suspending" {
		t.Fatal("uncertain publication lost its crash-recovery intent")
	}
	if _, err := s.payloads.read(current, nil); err != nil {
		t.Fatal("uncertain publication deleted a referenced payload")
	}
}

func TestSuspendIntentSavedProfileAndFakeClock(t *testing.T) {
	s, dir, live, _ := suspendIntentFixture(t)
	live.Persistent, live.ProfileName = true, "profile-a"
	if err := saveRecord(dir, live); err != nil {
		t.Fatal(err)
	}
	live, err := s.read(live.ID)
	if err != nil {
		t.Fatal(err)
	}
	sealer := &fakeSuspendSealer{}
	capture, err := prepareSuspendCapture(suspendTestManifest(), true, "profile-a", sealer)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.clear()
	now := suspendTestNow.Add(42 * time.Hour)
	intent, err := s.stage(suspendPagesContext(t), live, live.OwnerUserID, capture, now, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	expiry, err := suspendExpiry(intent)
	if err != nil || !expiry.Equal(now.Add(7*24*time.Hour)) {
		t.Fatal("persistent intent ignored its fake clock or TTL")
	}
	if _, err := s.payloads.read(intent, nil); err == nil {
		t.Fatal("saved intent could be read without an unlocked profile")
	}
	if got, err := s.payloads.read(intent, sealer); err != nil || len(got.Tabs) != 2 {
		t.Fatal("saved intent lost its sealed capture")
	}
}

func TestSuspendIntentRejectsDetachedPayloadDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not rename an open directory")
	}
	s, dir, live, capture := suspendIntentFixture(t)
	syncDir := s.payloads.syncDir
	s.payloads.syncDir = func() error {
		if err := syncDir(); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(dir, suspendManifestDirName), filepath.Join(dir, "detached-payloads")); err != nil {
			return err
		}
		return os.Mkdir(filepath.Join(dir, suspendManifestDirName), 0o700)
	}
	if out, err := s.stage(suspendPagesContext(t), live, live.OwnerUserID, capture, suspendTestNow, func() error { return nil }); err == nil || out.ID != "" {
		t.Fatal("detached manifest directory received an acknowledged intent")
	}
	current, err := loadRecord(dir, live.ID)
	if err != nil || current.State != RecordLive {
		t.Fatal("detached payload replaced live registry authority")
	}
}

func TestSuspendIntentRegistryByteBounds(t *testing.T) {
	if maxSuspendRegistryRecordBytes != 65536 || maxSuspendRegistryBytes != 8388608 || maxSuspendRegistryEntries != 1024 {
		t.Fatal("registry bounds changed without an explicit contract update")
	}
	for _, kind := range []string{"record-bytes", "total-bytes"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, live, capture := suspendIntentFixture(t)
			data, err := json.Marshal(live)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "record-bytes" {
				// Valid JSON, not a parser error masquerading as a byte bound.
				data = append(data, []byte(strings.Repeat(" ", maxSuspendRegistryRecordBytes+1-len(data)))...)
				if err := os.WriteFile(recordFile(dir, live.ID), data, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				data = make([]byte, maxSuspendRegistryRecordBytes)
				for i := 0; i < maxSuspendRegistryBytes/maxSuspendRegistryRecordBytes; i++ {
					if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf(".tmp-bytes-%d", i)), data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := s.stage(suspendPagesContext(t), live, live.OwnerUserID, capture, suspendTestNow, func() error { return nil }); err == nil {
				t.Fatal("oversized registry was admitted")
			}
		})
	}
}

func TestSuspendIntentReservesAtomicReplacementCapacity(t *testing.T) {
	for _, kind := range []string{"bytes", "entry", "entry-full"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, live, capture := suspendIntentFixture(t)
			data := []byte{}
			count := maxSuspendRegistryEntries - 3 // live file and payload dir use two
			if kind == "entry-full" {
				count++
			}
			if kind == "bytes" {
				data = make([]byte, maxSuspendRegistryRecordBytes)
				count = maxSuspendRegistryBytes/maxSuspendRegistryRecordBytes - 1
			}
			for i := 0; i < count; i++ {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf(".tmp-reserve-%d", i)), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// These files are below the absolute entry/byte caps, but the
			// incoming temporary record also has to fit physically.
			if kind == "entry" {
				if _, err := s.stage(suspendPagesContext(t), live, live.OwnerUserID, capture, suspendTestNow, func() error { return nil }); err != nil {
					t.Fatal("one free entry should admit a bounded atomic replacement")
				}
				return
			}
			if _, err := s.stage(suspendPagesContext(t), live, live.OwnerUserID, capture, suspendTestNow, func() error { return nil }); err == nil {
				t.Fatal("atomic replacement exceeded the reserved physical registry budget")
			}
		})
	}
}

func TestSuspendIntentCapacityAndUnparseableRecords(t *testing.T) {
	for _, kind := range []string{"owner-cap", "malformed", "oversize", "symlink", "public-file", "public-directory", "unknown-state", "wrong-id", "entry-cap"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, live, capture := suspendIntentFixture(t)
			switch kind {
			case "owner-cap":
				for i := 0; i < maxSuspendedPerOwner; i++ {
					r := suspendedTestRecord(t, false)
					r.ID = fmt.Sprintf("cb-%x", 100+i)
					if err := saveRecord(dir, r); err != nil {
						t.Fatal(err)
					}
				}
			case "malformed":
				if err := os.WriteFile(filepath.Join(dir, "cb-dead.json"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(filepath.Join(dir, "cb-dead.json"), []byte(strings.Repeat(" ", maxSuspendRegistryRecordBytes+1)), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(live.ID+".json", filepath.Join(dir, "cb-dead.json")); err != nil {
					t.Skip("symlinks unavailable")
				}
			case "public-file", "public-directory":
				if runtime.GOOS == "windows" {
					t.Skip("Unix permission regression")
				}
				path := recordFile(dir, live.ID)
				if kind == "public-directory" {
					path = dir
				}
				if err := os.Chmod(path, 0o755); err != nil {
					t.Fatal(err)
				}
			case "unknown-state", "wrong-id":
				r := live
				if kind == "unknown-state" {
					r.State = "unknown"
				} else {
					r.ID = "cb-bad"
				}
				data, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(recordFile(dir, live.ID), data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "entry-cap":
				for i := 0; i < maxSuspendRegistryEntries; i++ {
					if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf(".tmp-%d", i)), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := s.stage(suspendPagesContext(t), live, live.OwnerUserID, capture, suspendTestNow, func() error { return nil }); err == nil {
				t.Fatal("unsafe or incomplete registry accounting admitted a suspend intent")
			}
		})
	}
}

func TestSuspendIntentSerializesCompetingCaptures(t *testing.T) {
	s, _, live, capture := suspendIntentFixture(t)
	ctx := suspendPagesContext(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.stage(ctx, live, live.OwnerUserID, capture, suspendTestNow, func() error { return nil })
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal("competing captures both acknowledged the same live generation")
	}
}
