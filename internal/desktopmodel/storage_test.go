package desktopmodel

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func (s *privateStore) publishFixture(next validationState) (bool, error) {
	return s.publish(next, fixtureManifest(), nil)
}

func fixtureParent(t *testing.T) (*os.Root, string) {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, path
}

func fixtureStore(t *testing.T) (*privateStore, *os.Root) {
	t.Helper()
	p, _ := fixtureParent(t)
	s, err := freshStore(p, storageOps{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !s.closed {
			_ = s.close()
		}
	})
	return s, p
}

func storeBytes(t *testing.T, s *privateStore) []byte {
	t.Helper()
	b, err := s.root.ReadFile("state.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parentAlive(t *testing.T, p *os.Root) {
	t.Helper()
	f, err := p.Open(".")
	if err != nil {
		t.Fatal("caller parent was closed")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoragePublication(t *testing.T) {
	s, p := fixtureStore(t)
	initial, m := fixtureState(t)
	if got, err := s.read(); err != errStorage || got != (validationState{}) {
		t.Fatal("missing state adopted")
	}
	if confirmed, err := s.publishFixture(initial); err != nil || !confirmed {
		t.Fatal("initial publication", err)
	}
	info, err := s.root.Lstat("state.json")
	if err != nil || !privateInfo(info, false) {
		t.Fatal("private state mode")
	}
	if got, err := s.read(); err != nil || got != initial {
		t.Fatal("roundtrip", err)
	}
	checking, err := advanceState(initial, "checking", m, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if confirmed, err := s.publishFixture(checking); err != nil || !confirmed {
		t.Fatal("checking publication")
	}
	current, _ := s.root.Lstat("state.json")
	if os.SameFile(info, current) {
		t.Fatal("existing inode overwritten")
	}
	rejected, err := advanceState(checking, "rejected", m, nil, "archive_invalid")
	if err != nil {
		t.Fatal(err)
	}
	if confirmed, err := s.publishFixture(rejected); err != nil || !confirmed {
		t.Fatal("rejection publication")
	}
	if confirmed, err := s.publishFixture(checking); err != errState || confirmed {
		t.Fatal("terminal publication reset")
	}
	want := storeBytes(t, s)
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	parentAlive(t, p)
	if got, err := s.read(); err != errStorage || got != (validationState{}) {
		t.Fatal("closed read")
	}
	if ok, err := s.publishFixture(initial); err != errStorage || ok {
		t.Fatal("closed write")
	}
	if err := s.close(); err != errStorage {
		t.Fatal("double close")
	}
	root, err := p.OpenRoot(s.directory)
	if err != nil {
		t.Fatal("close removed store")
	}
	defer root.Close()
	got, err := root.ReadFile("state.json")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("close removed state")
	}
}

func TestStorageFirstAndLawfulState(t *testing.T) {
	s, _ := fixtureStore(t)
	idle, m := fixtureState(t)
	checking, _ := advanceState(idle, "checking", m, nil, "")
	if ok, err := s.publishFixture(checking); err != errState || ok {
		t.Fatal("first publish bypassed idle")
	}
	bad := idle
	bad.Sequence = 2
	if ok, err := s.publishFixture(bad); err != errState || ok {
		t.Fatal("first publish wrong sequence")
	}
	if _, err := s.root.Lstat("state.json"); !os.IsNotExist(err) {
		t.Fatal("illegal first wrote state")
	}
	if ok, err := s.publishFixture(idle); err != nil || !ok {
		t.Fatal(err)
	}
	before := storeBytes(t, s)
	for _, bad := range []validationState{
		idle, {1, idle.OperationID, idle.ManifestFingerprint, 3, "checking", ""},
		{1, strings.Repeat("b", 32), idle.ManifestFingerprint, 2, "checking", ""},
		{1, idle.OperationID, strings.Repeat("b", 64), 2, "checking", ""},
		{1, idle.OperationID, idle.ManifestFingerprint, 2, "validated", ""},
	} {
		if ok, err := s.publishFixture(bad); err != errState || ok {
			t.Fatal("unlawful state accepted")
		}
		if !bytes.Equal(before, storeBytes(t, s)) {
			t.Fatal("unlawful state changed publication")
		}
	}
}

func TestStoragePreRenameFaults(t *testing.T) {
	for _, stage := range []string{"destination", "file_open", "file_write", "file_sync", "file_close", "rename"} {
		t.Run(stage, func(t *testing.T) {
			s, _ := fixtureStore(t)
			idle, m := fixtureState(t)
			if _, err := s.publishFixture(idle); err != nil {
				t.Fatal(err)
			}
			before := storeBytes(t, s)
			checking, _ := advanceState(idle, "checking", m, nil, "")
			fired := false
			s.ops.before = func(at string) error {
				if at == stage {
					fired = true
					return errors.New("private detail")
				}
				return nil
			}
			if ok, err := s.publishFixture(checking); err != errStorage || ok || !fired {
				t.Fatal("pre-rename fault not refused")
			}
			s.ops.before = nil
			if s.uncertain || !bytes.Equal(before, storeBytes(t, s)) {
				t.Fatal("pre-rename failure changed state")
			}
			if got, err := s.read(); err != nil || got != idle {
				t.Fatal("prior state lost")
			}
			f, err := s.root.Open(".")
			if err != nil {
				t.Fatal(err)
			}
			names, err := f.Readdirnames(-1)
			_ = f.Close()
			if err != nil || len(names) != 1 || names[0] != "state.json" {
				t.Fatal("temporary file retained despite known identity", names)
			}
		})
	}
}

func TestStoragePostRenameUncertainty(t *testing.T) {
	for _, stage := range []string{"post_rename_identity", "directory_open", "directory_sync", "directory_close"} {
		t.Run(stage, func(t *testing.T) {
			s, _ := fixtureStore(t)
			idle, m := fixtureState(t)
			if _, err := s.publishFixture(idle); err != nil {
				t.Fatal(err)
			}
			oldInfo := s.expected
			checking, _ := advanceState(idle, "checking", m, nil, "")
			want, _ := checking.canonical()
			fired := false
			s.ops.before = func(at string) error {
				if at == stage {
					fired = true
					return errors.New("private detail")
				}
				return nil
			}
			if ok, err := s.publishFixture(checking); err != errUncertain || ok || !fired {
				t.Fatal("post-rename failure claimed certainty")
			}
			if !s.uncertain || s.expected == nil || os.SameFile(s.expected, oldInfo) {
				t.Fatal("new expected inode not retained")
			}
			if stage == "post_rename_identity" && s.observed != nil {
				t.Fatal("observed inode fabricated")
			}
			if stage != "post_rename_identity" && s.observed == nil {
				t.Fatal("actual observation lost")
			}
			if !bytes.Equal(want, storeBytes(t, s)) {
				t.Fatal("new publication rolled back")
			}
			if got, err := s.read(); err != errUncertain || got != (validationState{}) {
				t.Fatal("uncertain read claimed state")
			}
			s.ops.before = nil
			if ok, err := s.publishFixture(checking); err != errUncertain || ok {
				t.Fatal("uncertainty auto-repaired")
			}
		})
	}
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "different_inode"}[replace], func(t *testing.T) {
			s, _ := fixtureStore(t)
			idle, _ := fixtureState(t)
			s.ops.before = func(at string) error {
				if at == "post_rename_identity" {
					if replace {
						// Retain the original inode: unlink/recreate can reuse its
						// number and is not a distinguishing identity fixture.
						if err := s.root.Rename("state.json", "original"); err != nil {
							t.Fatal(err)
						}
						if err := s.root.WriteFile("state.json", []byte("different"), 0600); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := s.root.Remove("state.json"); err != nil {
							t.Fatal(err)
						}
					}
				}
				return nil
			}
			if ok, err := s.publishFixture(idle); err != errUncertain || ok || s.expected == nil {
				t.Fatal("identity failure not uncertain")
			}
			if replace != (s.observed != nil) {
				t.Fatal("observed identity provenance")
			}
		})
	}
}

func TestStorageSubstitutionAndSentinels(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := fixtureStore(t)
			outside := t.TempDir() + "/sentinel"
			sentinel := []byte("never modified")
			if err := os.WriteFile(outside, sentinel, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file":
				if err := s.root.WriteFile("state.json", sentinel, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := s.root.Symlink(outside, "state.json"); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(outside, s.root.Name()+"/state.json"); err != nil {
					t.Fatal(err)
				}
			}
			idle, _ := fixtureState(t)
			if ok, err := s.publishFixture(idle); err != errStorage || ok {
				t.Fatal("existing state adopted")
			}
			got, err := os.ReadFile(outside)
			if err != nil || !bytes.Equal(got, sentinel) {
				t.Fatal("outside target modified")
			}
		})
	}
	for _, kind := range []string{"replacement", "symlink", "mode", "oversized", "corrupt"} {
		t.Run("published_"+kind, func(t *testing.T) {
			s, _ := fixtureStore(t)
			idle, m := fixtureState(t)
			if _, err := s.publishFixture(idle); err != nil {
				t.Fatal(err)
			}
			before := storeBytes(t, s)
			switch kind {
			case "replacement":
				if err := s.root.Rename("state.json", "old"); err != nil {
					t.Fatal(err)
				}
				if err := s.root.WriteFile("state.json", before, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := s.root.Rename("state.json", "old"); err != nil {
					t.Fatal(err)
				}
				if err := s.root.Symlink("old", "state.json"); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := s.root.Chmod("state.json", 0644); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := s.root.WriteFile("state.json", bytes.Repeat([]byte(" "), stateLimit+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := s.root.WriteFile("state.json", []byte("not state"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			checking, _ := advanceState(idle, "checking", m, nil, "")
			if got, err := s.read(); err != errStorage || got != (validationState{}) {
				t.Fatal("substituted read admitted")
			}
			if ok, err := s.publishFixture(checking); err != errStorage || ok {
				t.Fatal("substituted publication admitted")
			}
		})
	}
}

func TestStorageExclusiveCollision(t *testing.T) {
	s, _ := fixtureStore(t)
	name := strings.Repeat("42", 16)
	sentinel := []byte("unmodified collision")
	if err := s.root.WriteFile(name, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	s.ops.entropy = bytes.NewReader(bytes.Repeat([]byte{0x42}, 16*8))
	idle, _ := fixtureState(t)
	if ok, err := s.publishFixture(idle); err != errStorage || ok {
		t.Fatal("exclusive creation bypassed")
	}
	got, err := s.root.ReadFile(name)
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Fatal("collision inode written or removed")
	}
	if _, err := s.root.Lstat("state.json"); !os.IsNotExist(err) {
		t.Fatal("collision published state")
	}
	p, _ := fixtureParent(t)
	if err := p.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	if s, err := freshStore(p, storageOps{entropy: bytes.NewReader(bytes.Repeat([]byte{0x42}, 16*8))}); err != errStorage || s != nil {
		t.Fatal("existing directory adopted")
	}
	parentAlive(t, p)
}

func TestStorageFactoryOwnership(t *testing.T) {
	for _, stage := range []string{"parent_open", "parent_close", "child_mkdir", "child_lstat", "child_open", "child_identity"} {
		t.Run(stage, func(t *testing.T) {
			p, _ := fixtureParent(t)
			fired := false
			ops := storageOps{before: func(at string) error {
				if at == stage {
					fired = true
					return errors.New("private")
				}
				return nil
			}}
			if s, err := freshStore(p, ops); err != errStorage || s != nil || !fired {
				t.Fatal("factory fault")
			}
			parentAlive(t, p)
		})
	}
	for _, mode := range []os.FileMode{0755, 0770, os.ModeSticky | 0700, os.ModeSetgid | 0700} {
		t.Run(mode.String(), func(t *testing.T) {
			p, path := fixtureParent(t)
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if s, err := freshStore(p, storageOps{}); err != errStorage || s != nil {
				t.Fatal("nonprivate parent accepted")
			}
			parentAlive(t, p)
		})
	}
	if s, err := freshStore(nil, storageOps{}); err != errStorage || s != nil {
		t.Fatal("nil parent")
	}
	p, _ := fixtureParent(t)
	if s, err := freshStore(p, storageOps{entropy: failedReader{}}); err != errStorage || s != nil {
		t.Fatal("entropy failure")
	}
	parentAlive(t, p)
	for _, platform := range []string{"linux", "darwin", "windows", "js", "plan9", "wasip1"} {
		if supportedStorage(platform) != (platform == "linux" || platform == "darwin") {
			t.Fatal("unsupported root semantics enabled")
		}
	}
}

func TestStorageFactorySubstitution(t *testing.T) {
	for _, kind := range []string{"outside_link", "inside_link", "replacement"} {
		t.Run(kind, func(t *testing.T) {
			p, _ := fixtureParent(t)
			name := strings.Repeat("11", 16)
			outside := t.TempDir()
			if err := p.Mkdir("inside", 0700); err != nil {
				t.Fatal(err)
			}
			stage := "child_open"
			if kind == "replacement" {
				stage = "child_identity"
			}
			ops := storageOps{entropy: bytes.NewReader(bytes.Repeat([]byte{0x11}, 16)), before: func(at string) error {
				if at == stage {
					if err := p.Rename(name, "original"); err != nil {
						t.Fatal(err)
					}
					switch kind {
					case "outside_link":
						if err := p.Symlink(outside, name); err != nil {
							t.Fatal(err)
						}
					case "inside_link":
						if err := p.Symlink("inside", name); err != nil {
							t.Fatal(err)
						}
					case "replacement":
						if err := p.Mkdir(name, 0700); err != nil {
							t.Fatal(err)
						}
					}
				}
				return nil
			}}
			if s, err := freshStore(p, ops); err != errStorage || s != nil {
				t.Fatal("factory adopted substituted root")
			}
			parentAlive(t, p)
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatal("outside root mutated")
			}
		})
	}
}

func TestStorageAnchoringAndConcurrentWriters(t *testing.T) {
	s, p := fixtureStore(t)
	idle, m := fixtureState(t)
	if err := p.Rename(s.directory, "moved"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.publishFixture(idle); err != nil || !ok {
		t.Fatal("descriptor lost after rename")
	}
	checking, _ := advanceState(idle, "checking", m, nil, "")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.publishFixture(checking); results <- err }()
	}
	wg.Wait()
	close(results)
	success, refused := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if err == errState {
			refused++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || refused != 1 {
		t.Fatal("concurrent transitions not serialized")
	}
	if got, err := s.read(); err != nil || got != checking {
		t.Fatal("wrong concurrent state")
	}
	parentAlive(t, p)
}

func TestStorageReceiptRequiredAtPublication(t *testing.T) {
	s, _ := fixtureStore(t)
	b := makeGzip(t, makeTar(t, []tarMember{{"fixture", []byte("benign"), 0}}))
	m := manifestFor(b)
	idle, err := newState(strings.Repeat("a", 32), m)
	if err != nil {
		t.Fatal(err)
	}
	wrongInitial := idle
	wrongInitial.ManifestFingerprint = strings.Repeat("0", 64)
	if ok, err := s.publish(wrongInitial, m, nil); err != errState || ok {
		t.Fatal("initial descriptor binding bypassed")
	}
	if ok, err := s.publish(idle, m, nil); err != nil || !ok {
		t.Fatal(err)
	}
	checking, err := advanceState(idle, "checking", m, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.publish(checking, m, nil); err != nil || !ok {
		t.Fatal(err)
	}
	before := storeBytes(t, s)
	old := s.expected
	fabricated := checking
	fabricated.Status = "validated"
	fabricated.Sequence++
	receipt, err := validateArchive(bytes.NewReader(b), m)
	if err != nil {
		t.Fatal(err)
	}
	wrongDescriptor := m
	wrongDescriptor.CompressedBytes++
	for name, input := range map[string]struct {
		m manifest
		r *archiveReceipt
	}{
		"fabricated_state": {m, nil}, "zero_receipt": {m, &archiveReceipt{}},
		"wrong_sha":         {m, &archiveReceipt{receipt.fingerprint, strings.Repeat("0", 64), 1, 1}},
		"wrong_fingerprint": {m, &archiveReceipt{strings.Repeat("0", 64), receipt.archiveSHA, 1, 1}},
		"wrong_descriptor":  {wrongDescriptor, receipt},
	} {
		t.Run(name, func(t *testing.T) {
			if ok, err := s.publish(fabricated, input.m, input.r); err != errState || ok {
				t.Fatal("fabricated validation published")
			}
			if !bytes.Equal(before, storeBytes(t, s)) || !os.SameFile(old, s.expected) {
				t.Fatal("forbidden publication changed prior inode/bytes")
			}
			f, err := s.root.Open(".")
			if err != nil {
				t.Fatal(err)
			}
			names, err := f.Readdirnames(-1)
			_ = f.Close()
			if err != nil || len(names) != 1 || names[0] != "state.json" {
				t.Fatal("invalid receipt created entries")
			}
		})
	}
	validated, err := advanceState(checking, "validated", m, receipt, "")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.publish(validated, m, receipt); err != nil || !ok {
		t.Fatal("actual fully validated receipt refused", err)
	}
}

func TestStorageCleanupFailureDoesNotAccumulate(t *testing.T) {
	s, _ := fixtureStore(t)
	idle, m := fixtureState(t)
	if ok, err := s.publishFixture(idle); err != nil || !ok {
		t.Fatal(err)
	}
	before := storeBytes(t, s)
	checking, _ := advanceState(idle, "checking", m, nil, "")
	s.ops.before = func(at string) error {
		if at == "file_write" || at == "temporary_remove" {
			return errors.New("private")
		}
		return nil
	}
	if ok, err := s.publishFixture(checking); err != errStorage || ok || !s.cleanupPending || s.uncertain {
		t.Fatal("cleanup failure misreported")
	}
	s.ops.before = nil
	if got, err := s.read(); err != nil || got != idle || !bytes.Equal(before, storeBytes(t, s)) {
		t.Fatal("prior confirmed state not preserved")
	}
	f, err := s.root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil || len(names) != 2 {
		t.Fatal("owned failed temp not retained")
	}
	if ok, err := s.publishFixture(checking); err != errStorage || ok {
		t.Fatal("cleanup failure auto-retried")
	}
	f, err = s.root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil || len(after) != len(names) {
		t.Fatal("cleanup failure accumulated more files")
	}
}

func TestStorageActualOperationOrder(t *testing.T) {
	s, _ := fixtureStore(t)
	idle, _ := fixtureState(t)
	var actual []string
	s.ops.before = func(at string) error {
		if at == "file_sync" {
			actual = append(actual, "sync_event")
		}
		return nil
	}
	s.ops.writeFn = func(f *os.File, b []byte) (int, error) { actual = append(actual, "write"); return f.Write(b) }
	s.ops.syncFn = func(f *os.File) error { actual = append(actual, "actual_file_sync"); return f.Sync() }
	s.ops.closeFn = func(f *os.File) error { actual = append(actual, "actual_file_close"); return f.Close() }
	s.ops.renameFn = func(r *os.Root, old, new string) error {
		actual = append(actual, "actual_rename")
		return r.Rename(old, new)
	}
	s.ops.directorySyncFn = func(f *os.File) error { actual = append(actual, "actual_directory_sync"); return f.Sync() }
	s.ops.directoryCloseFn = func(f *os.File) error { actual = append(actual, "actual_directory_close"); return f.Close() }
	if ok, err := s.publishFixture(idle); err != nil || !ok {
		t.Fatal(err)
	}
	want := []string{"write", "sync_event", "actual_file_sync", "actual_file_close", "actual_rename", "actual_directory_sync", "actual_directory_close"}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("actual operations/order %v, want %v", actual, want)
	}
}

func TestStorageActualOperationFailures(t *testing.T) {
	for _, operation := range []string{"write_partial", "write_short", "sync", "close", "rename", "directory_sync", "directory_close"} {
		t.Run(operation, func(t *testing.T) {
			s, _ := fixtureStore(t)
			idle, m := fixtureState(t)
			if ok, err := s.publishFixture(idle); err != nil || !ok {
				t.Fatal(err)
			}
			before := storeBytes(t, s)
			checking, _ := advanceState(idle, "checking", m, nil, "")
			fired := false
			private := errors.New("private detail")
			switch operation {
			case "write_partial":
				s.ops.writeFn = func(f *os.File, b []byte) (int, error) {
					fired = true
					n, err := f.Write(b[:len(b)/2])
					if err != nil {
						return n, err
					}
					return n, private
				}
			case "write_short":
				s.ops.writeFn = func(f *os.File, b []byte) (int, error) { fired = true; return f.Write(b[:len(b)/2]) }
			case "sync":
				s.ops.syncFn = func(*os.File) error { fired = true; return private }
			case "close":
				s.ops.closeFn = func(f *os.File) error { fired = true; _ = f.Close(); return private }
			case "rename":
				s.ops.renameFn = func(*os.Root, string, string) error { fired = true; return private }
			case "directory_sync":
				s.ops.directorySyncFn = func(*os.File) error { fired = true; return private }
			case "directory_close":
				s.ops.directoryCloseFn = func(f *os.File) error { fired = true; _ = f.Close(); return private }
			}
			ok, err := s.publishFixture(checking)
			post := strings.HasPrefix(operation, "directory_")
			if !fired || ok {
				t.Fatal("actual failing operation was omitted or claimed success")
			}
			if post {
				if err != errUncertain || !s.uncertain {
					t.Fatal("actual post-rename failure not uncertain")
				}
				want, _ := checking.canonical()
				if !bytes.Equal(want, storeBytes(t, s)) {
					t.Fatal("backend-like publication rollback claimed")
				}
			} else {
				if err != errStorage || s.uncertain || !bytes.Equal(before, storeBytes(t, s)) {
					t.Fatal("actual pre-rename failure changed confirmed state")
				}
			}
		})
	}
}

func TestStorageParentPathRename(t *testing.T) {
	container := t.TempDir()
	path := container + "/parent"
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := os.Rename(path, container+"/moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := freshStore(parent, storageOps{})
	if err != nil {
		t.Fatal("supplied parent capability was re-resolved", err)
	}
	defer s.close()
	idle, _ := fixtureState(t)
	if ok, err := s.publishFixture(idle); err != nil || !ok {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatal("new pathname was adopted")
	}
	if _, err := os.Stat(container + "/moved/" + s.directory + "/state.json"); err != nil {
		t.Fatal("anchored publication absent")
	}
	parentAlive(t, parent)
}

func TestStorageNewModeMismatch(t *testing.T) {
	t.Run("child", func(t *testing.T) {
		parent, _ := fixtureParent(t)
		name := strings.Repeat("33", 16)
		ops := storageOps{entropy: bytes.NewReader(bytes.Repeat([]byte{0x33}, 16)), before: func(at string) error {
			if at == "child_lstat" {
				return parent.Chmod(name, 0500)
			}
			return nil
		}}
		if s, err := freshStore(parent, ops); err != errStorage || s != nil {
			t.Fatal("new child mode mismatch repaired/accepted")
		}
		parentAlive(t, parent)
	})
	t.Run("temporary", func(t *testing.T) {
		s, _ := fixtureStore(t)
		idle, m := fixtureState(t)
		if _, err := s.publishFixture(idle); err != nil {
			t.Fatal(err)
		}
		before := storeBytes(t, s)
		name := strings.Repeat("44", 16)
		s.ops.entropy = bytes.NewReader(bytes.Repeat([]byte{0x44}, 16))
		s.ops.before = func(at string) error {
			if at == "file_identity" {
				return s.root.Chmod(name, 0400)
			}
			return nil
		}
		checking, _ := advanceState(idle, "checking", m, nil, "")
		if ok, err := s.publishFixture(checking); err != errStorage || ok || !s.cleanupPending || s.uncertain {
			t.Fatal("new temporary mode mismatch accepted")
		}
		s.ops.before = nil
		if !bytes.Equal(before, storeBytes(t, s)) {
			t.Fatal("bad temp promoted")
		}
		info, err := s.root.Lstat(name)
		if err != nil || info.Mode() != 0400 {
			t.Fatal("bad temp permissions silently repaired")
		}
	})
}

func TestStorageFinalDestinationFence(t *testing.T) {
	s, _ := fixtureStore(t)
	idle, m := fixtureState(t)
	if ok, err := s.publishFixture(idle); err != nil || !ok {
		t.Fatal(err)
	}
	before := storeBytes(t, s)
	checking, _ := advanceState(idle, "checking", m, nil, "")
	calls := 0
	swapped := false
	s.ops.before = func(at string) error {
		if at == "destination" {
			calls++
			if calls == 3 {
				if err := s.root.Rename("state.json", "original"); err != nil {
					t.Fatal(err)
				}
				if err := s.root.WriteFile("state.json", before, 0600); err != nil {
					t.Fatal(err)
				}
				swapped = true
			}
		}
		return nil
	}
	if ok, err := s.publishFixture(checking); err != errStorage || ok || !swapped {
		t.Fatal("final remembered-inode fence omitted")
	}
	s.ops.before = nil
	if !bytes.Equal(before, storeBytes(t, s)) || s.last != idle {
		t.Fatal("substituted entry was published over")
	}
	original, err := s.root.ReadFile("original")
	if err != nil || !bytes.Equal(original, before) {
		t.Fatal("original confirmed bytes modified")
	}
}
