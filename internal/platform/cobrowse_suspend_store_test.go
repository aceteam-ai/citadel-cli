package platform

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type fakeSuspendSealer struct {
	id         string
	generation uint64
	plain      []byte
	input      []byte
	err        error
}

func (*fakeSuspendSealer) SuspendProfileName() string { return "profile-a" }

func (s *fakeSuspendSealer) SealSuspendManifest(id string, generation uint64, plain []byte) ([]byte, error) {
	s.id, s.generation, s.input = id, generation, plain
	s.plain = append([]byte(nil), plain...)
	return []byte("synthetic-opaque-ciphertext"), s.err
}
func (s *fakeSuspendSealer) UnsealSuspendManifest(id string, generation uint64, blob []byte) ([]byte, error) {
	if s.err != nil {
		return []byte("partial-private"), s.err
	}
	if id != s.id || generation != s.generation || string(blob) != "synthetic-opaque-ciphertext" {
		return nil, errors.New("synthetic private provider refusal")
	}
	return append([]byte(nil), s.plain...), nil
}

func testSuspendStore(t *testing.T) (*suspendManifestStore, string) {
	t.Helper()
	reg := filepath.Join(t.TempDir(), "registry")
	s, err := openSuspendManifestStore(reg, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.close() })
	return s, reg
}

func captureRecord(t *testing.T, m suspendManifest, persistent bool, sealer suspendManifestSealer) (sessionRecord, preparedSuspendCapture) {
	t.Helper()
	profileName := ""
	if persistent {
		profileName = "profile-a"
	}
	p, err := prepareSuspendCapture(m, persistent, profileName, sealer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.clear)
	live := suspendTestLive()
	live.ID, live.Generation = m.SessionID, m.Generation
	if persistent {
		live.Persistent = true
		live.ProfileName = "profile-a"
	}
	r, err := planSuspendedRecord(live, live.OwnerUserID, live.ProfileName, len(m.Tabs), int64(len(p.data)), nil, suspendTestNow)
	if err != nil {
		t.Fatal(err)
	}
	return r, p
}

func TestSuspendStorePrivateRoundTripAndCreateOnly(t *testing.T) {
	s, reg := testSuspendStore(t)
	m := suspendTestManifest()
	r, p := captureRecord(t, m, false, nil)
	if err := s.write(r, p); err != nil {
		t.Fatal(err)
	}
	got, err := s.read(r, nil)
	if err != nil || got.Tabs[0] != m.Tabs[0] {
		t.Fatal("stored manifest changed")
	}
	name, err := suspendManifestName(r)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(reg, suspendManifestDirName, name))
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatal("manifest is not an owner-only regular file")
	}
	if err := s.write(r, p); err == nil {
		t.Fatal("a retained generation was overwritten")
	}
	if _, err := s.read(r, nil); err != nil {
		t.Fatal("duplicate publication damaged prior capture")
	}
	if strings.Contains(fmt.Sprintf("%#v %v", p, s), "private-sentinel") {
		t.Fatal("diagnostic leaked tab data")
	}
	_ = s.close()
	reopened, err := openSuspendManifestStore(reg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if got, err := reopened.read(r, nil); err != nil || len(got.Tabs) != 2 {
		t.Fatal("process-restart read lost the manifest")
	}
	if err := reopened.remove(r); err != nil {
		t.Fatal(err)
	}
	if err := reopened.remove(r); err != nil {
		t.Fatal("already removed manifest was not idempotent")
	}
}

func TestSuspendStorePersistentUnlockAndCaptureGeneration(t *testing.T) {
	s, reg := testSuspendStore(t)
	m := suspendTestManifest()
	if _, err := prepareSuspendCapture(m, true, "profile-a", nil); err == nil {
		t.Fatal("persistent capture fell back to plaintext")
	}
	sealer := &fakeSuspendSealer{}
	r, p := captureRecord(t, m, true, sealer)
	if !bytes.Equal(sealer.input, make([]byte, len(sealer.input))) {
		t.Fatal("prepared plaintext was not cleared")
	}
	if err := s.write(r, p); err != nil {
		t.Fatal(err)
	}
	name, _ := suspendManifestName(r)
	blob, err := os.ReadFile(filepath.Join(reg, suspendManifestDirName, name))
	if err != nil || bytes.Contains(blob, []byte("private-sentinel")) || string(blob) != "synthetic-opaque-ciphertext" {
		t.Fatal("store did not use sealed bytes")
	}
	if _, err := s.read(r, nil); err == nil {
		t.Fatal("saved tabs read without unlock")
	}
	if got, err := s.read(r, sealer); err != nil || got.Tabs[0].URL != m.Tabs[0].URL {
		t.Fatal("unlocked read failed")
	}
	resuming, err := planReopenRecord(r, r.OwnerUserID, true, suspendTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if resuming.Generation != r.Generation+1 || resuming.Suspension.CaptureGeneration != r.Generation {
		t.Fatal("capture fence changed with the live generation")
	}
	if got, err := s.read(resuming, sealer); err != nil || got.Generation != r.Generation {
		t.Fatal("interrupted reopen cannot recover the old capture")
	}
	sealer.err = errors.New("private-sentinel")
	if _, err := s.read(r, sealer); err == nil || strings.Contains(err.Error(), "private-sentinel") {
		t.Fatal("provider refusal was accepted or exposed private details")
	}
}

func TestSuspendStoreRefusesMismatchedCapture(t *testing.T) {
	s, _ := testSuspendStore(t)
	r, p := captureRecord(t, suspendTestManifest(), false, nil)
	for _, change := range []func(*preparedSuspendCapture){
		func(p *preparedSuspendCapture) { p.id = "cb-aaaaaaaaaaaaaaaa" },
		func(p *preparedSuspendCapture) { p.generation++ },
		func(p *preparedSuspendCapture) { p.persistent = true },
		func(p *preparedSuspendCapture) { p.tabs++ },
		func(p *preparedSuspendCapture) { p.data = nil },
		func(p *preparedSuspendCapture) { p.data = p.data[:len(p.data)-1] },
	} {
		bad := p
		change(&bad)
		if err := s.write(r, bad); err == nil {
			t.Fatal("capture/record mismatch published")
		}
	}
	for _, change := range []func(*sessionRecord){
		func(r *sessionRecord) { r.ID = "../../escape" },
		func(r *sessionRecord) { r.State = RecordLive },
		func(r *sessionRecord) { r.Suspension = nil },
		func(r *sessionRecord) { r.OwnerUserID = "foreign" },
		func(r *sessionRecord) { r.Generation++ },
	} {
		bad := r
		change(&bad)
		if err := s.write(bad, p); err == nil {
			t.Fatal("invalid record published")
		}
		if _, err := s.read(bad, nil); err == nil {
			t.Fatal("invalid record read")
		}
		if err := s.remove(bad); err == nil {
			t.Fatal("invalid record removed a file")
		}
	}
	entries, err := os.ReadDir(s.root.Name())
	if err != nil || len(entries) != 1 || entries[0].Name() != suspendStoreLockName {
		t.Fatal("invalid request touched storage")
	}
}

func TestSuspendStoreSyncBeforePublicationAndFailureCleanup(t *testing.T) {
	for _, failure := range []string{"sync", "publish", "none"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := testSuspendStore(t)
			r, p := captureRecord(t, suspendTestManifest(), false, nil)
			synced, published := false, false
			s.syncFile = func(f *os.File) error {
				info, err := f.Stat()
				if err != nil || info.Size() != int64(len(p.data)) {
					t.Fatal("sync before complete write")
				}
				if failure == "sync" {
					return errors.New("private-sentinel")
				}
				synced = true
				return f.Sync()
			}
			s.publish = func(from, to string) error {
				if !synced {
					t.Fatal("publish before durable payload")
				}
				published = true
				if failure == "publish" {
					return errors.New("private-sentinel")
				}
				return s.root.Link(from, to)
			}
			err := s.write(r, p)
			if (err != nil) != (failure != "none") {
				t.Fatal("incorrect persistence result")
			}
			if err != nil && strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("private provider error escaped")
			}
			if failure == "sync" && published {
				t.Fatal("sync refusal still published")
			}
			entries, err := os.ReadDir(s.root.Name())
			want := 1 // stable zero-byte OS ownership lock
			if failure == "none" {
				want = 2
			}
			if err != nil || len(entries) != want {
				t.Fatal("failed operation leaked a temp/final file")
			}
		})
	}
}

func TestSuspendStoreRejectsUnsafeFilesystemObjects(t *testing.T) {
	s, _ := testSuspendStore(t)
	r, p := captureRecord(t, suspendTestManifest(), false, nil)
	name, _ := suspendManifestName(r)
	outside := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(outside, p.data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.root.Name(), name)); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation unavailable to this Windows test user")
		}
		t.Fatal(err)
	}
	if _, err := s.read(r, nil); err == nil {
		t.Fatal("symlinked manifest was read")
	}
	if err := s.write(r, p); err == nil {
		t.Fatal("symlinked manifest was overwritten")
	}
	if err := s.remove(r); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(outside); err != nil || !bytes.Equal(got, p.data) {
		t.Fatal("manifest deletion followed a symlink")
	}
	if err := os.Mkdir(filepath.Join(s.root.Name(), name), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read(r, nil); err == nil {
		t.Fatal("directory accepted as payload")
	}
	if err := s.write(r, p); err == nil {
		t.Fatal("directory accepted for capacity accounting")
	}
	if err := s.remove(r); err == nil {
		t.Fatal("unexpected directory was removed")
	}
}

func TestSuspendStoreReadBoundsAndMetadata(t *testing.T) {
	for _, kind := range []string{"oversize", "truncated", "malformed", "foreign-generation", "tab-count"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := testSuspendStore(t)
			r, p := captureRecord(t, suspendTestManifest(), false, nil)
			if err := s.write(r, p); err != nil {
				t.Fatal(err)
			}
			name, _ := suspendManifestName(r)
			switch kind {
			case "oversize":
				if err := os.WriteFile(filepath.Join(s.root.Name(), name), make([]byte, maxSuspendManifestBytes+4097), 0o600); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				if err := os.Truncate(filepath.Join(s.root.Name(), name), 1); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(filepath.Join(s.root.Name(), name), bytes.Repeat([]byte("x"), len(p.data)), 0o600); err != nil {
					t.Fatal(err)
				}
			case "foreign-generation":
				r.Generation++
				r.Suspension.CaptureGeneration++
			case "tab-count":
				r.Suspension.TabCount++
			}
			if _, err := s.read(r, nil); err == nil {
				t.Fatal("invalid stored payload accepted")
			}
		})
	}
}

func TestSuspendStoreCountsOrphansAndConcurrentWrites(t *testing.T) {
	s, _ := testSuspendStore(t)
	r, p := captureRecord(t, suspendTestManifest(), false, nil)
	remaining := maxSuspendedNodeBytes - int64(len(p.data))
	for i := 0; remaining > 0; i++ {
		n := min(remaining, int64(maxSuspendManifestBytes+4096))
		f, err := os.OpenFile(filepath.Join(s.root.Name(), fmt.Sprintf("orphan-%d", i)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		err = f.Truncate(n)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		remaining -= n
	}
	m2 := suspendTestManifest()
	m2.SessionID = "cb-aaaaaaaaaaaaaaaa"
	r2, p2 := captureRecord(t, m2, false, nil)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, request := range []struct {
		r sessionRecord
		p preparedSuspendCapture
	}{{r, p}, {r2, p2}} {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.write(request.r, request.p) }()
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatal("orphan accounting or concurrent capacity fence lost")
	}
	used, err := s.usage()
	if err != nil || used != maxSuspendedNodeBytes {
		t.Fatal("exact byte bound not honored")
	}
}

func TestSuspendStoreReadOnlyOpenAndPrivacy(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := openSuspendManifestStore(missing, false); err == nil {
		t.Fatal("missing store unexpectedly opened")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("read-only open created state")
	}
	if runtime.GOOS == "windows" {
		return
	} // private Windows DACL is enforced by the existing base-dir helper.
	s, reg := testSuspendStore(t)
	r, p := captureRecord(t, suspendTestManifest(), false, nil)
	if err := s.write(r, p); err != nil {
		t.Fatal(err)
	}
	name, _ := suspendManifestName(r)
	if err := os.Chmod(filepath.Join(s.root.Name(), name), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read(r, nil); err == nil {
		t.Fatal("public-readable file accepted")
	}
	if err := os.Chmod(filepath.Join(s.root.Name(), name), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.root.Name(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := openSuspendManifestStore(reg, true); err == nil {
		t.Fatal("public-readable store silently trusted or repaired")
	}
	if _, err := s.read(r, nil); err == nil {
		t.Fatal("an open store ignored changed directory privacy")
	}
	m2 := suspendTestManifest()
	m2.SessionID = "cb-bbbbbbbbbbbbbbbb"
	r2, p2 := captureRecord(t, m2, false, nil)
	if err := s.write(r2, p2); err == nil {
		t.Fatal("an open store wrote into a no-longer-private directory")
	}
}

func TestSuspendStoreExclusiveAcrossInstances(t *testing.T) {
	s, reg := testSuspendStore(t)
	for _, create := range []bool{false, true} {
		if other, err := openSuspendManifestStore(reg, create); err == nil {
			_ = other.close()
			t.Fatal("another manager acquired live manifest storage")
		}
	}
	_ = s.close()
	other, err := openSuspendManifestStore(reg, false)
	if err != nil {
		t.Fatal("closed owner did not release OS lock")
	}
	defer other.close()
}

func TestSuspendStoreDirectorySyncFailureRemainsAccounted(t *testing.T) {
	s, _ := testSuspendStore(t)
	r, p := captureRecord(t, suspendTestManifest(), false, nil)
	realSync := s.syncDir
	s.syncDir = func() error { return errors.New("private-sentinel") }
	if err := s.write(r, p); err == nil || strings.Contains(err.Error(), "private-sentinel") {
		t.Fatal("uncertain publication reported success or exposed details")
	}
	used, err := s.usage()
	if err != nil || used != int64(len(p.data)) {
		t.Fatal("uncertain publication became free storage")
	}
	if err := s.remove(r); err == nil {
		t.Fatal("uncertain removal reported success")
	}
	if err := s.remove(r); err == nil {
		t.Fatal("removal retry bypassed required directory sync")
	}
	s.syncDir = realSync
	if err := s.remove(r); err != nil {
		t.Fatal("confirmed absence did not recover")
	}
}

func TestSuspendStoreBoundsDirectoryEnumeration(t *testing.T) {
	s, _ := testSuspendStore(t)
	r, p := captureRecord(t, suspendTestManifest(), false, nil)
	for i := 0; i < maxSuspendStoreEntries; i++ {
		if err := os.WriteFile(filepath.Join(s.root.Name(), fmt.Sprintf("empty-%d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.write(r, p); err == nil {
		t.Fatal("unbounded directory accepted")
	}
}
