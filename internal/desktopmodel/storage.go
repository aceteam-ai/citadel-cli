package desktopmodel

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"sync"
)

// The parent capability is caller-owned and trusted, not a UID/consent proof.
// The child is fresh; this package intentionally cannot reopen/adopt a store.
type storageOps struct {
	entropy          io.Reader
	before           func(string) error
	writeFn          func(*os.File, []byte) (int, error)
	syncFn           func(*os.File) error
	closeFn          func(*os.File) error
	renameFn         func(*os.Root, string, string) error
	directorySyncFn  func(*os.File) error
	directoryCloseFn func(*os.File) error
}

func (o storageOps) writeFile(f *os.File, b []byte) (int, error) {
	if o.writeFn != nil {
		return o.writeFn(f, b)
	}
	return f.Write(b)
}
func (o storageOps) syncFile(f *os.File) error {
	if o.syncFn != nil {
		return o.syncFn(f)
	}
	return f.Sync()
}
func (o storageOps) closeFile(f *os.File) error {
	if o.closeFn != nil {
		return o.closeFn(f)
	}
	return f.Close()
}
func (o storageOps) rename(r *os.Root, old, new string) error {
	if o.renameFn != nil {
		return o.renameFn(r, old, new)
	}
	return r.Rename(old, new)
}
func (o storageOps) syncDirectory(f *os.File) error {
	if o.directorySyncFn != nil {
		return o.directorySyncFn(f)
	}
	return f.Sync()
}
func (o storageOps) closeDirectory(f *os.File) error {
	if o.directoryCloseFn != nil {
		return o.directoryCloseFn(f)
	}
	return f.Close()
}

func (o storageOps) event(stage string) error {
	if o.before != nil {
		return o.before(stage)
	}
	return nil
}

func (o storageOps) name() (string, error) {
	r := o.entropy
	if r == nil {
		r = rand.Reader
	}
	var b [16]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", errStorage
	}
	return hex.EncodeToString(b[:]), nil
}

func privateInfo(i os.FileInfo, directory bool) bool {
	if i == nil {
		return false
	}
	if directory {
		return i.Mode() == os.ModeDir|0700
	}
	return i.Mode() == 0600
}

func supportedStorage(platform string) bool { return platform == "linux" || platform == "darwin" }

type privateStore struct {
	mu             sync.Mutex
	root           *os.Root
	directory      string
	directoryInfo  os.FileInfo
	ops            storageOps
	closed         bool
	uncertain      bool
	cleanupPending bool
	last           validationState
	expected       os.FileInfo
	observed       os.FileInfo
}

func freshStore(parent *os.Root, ops storageOps) (*privateStore, error) {
	if !supportedStorage(runtime.GOOS) {
		return nil, errUnsupported
	}
	if parent == nil || ops.event("parent_open") != nil {
		return nil, errStorage
	}
	f, err := parent.Open(".")
	if err != nil {
		return nil, errStorage
	}
	info, statErr := f.Stat()
	hookErr := ops.event("parent_close")
	closeErr := f.Close()
	if statErr != nil || hookErr != nil || closeErr != nil || !privateInfo(info, true) {
		return nil, errStorage
	}
	var name string
	created := false
	for attempt := 0; attempt < 8; attempt++ {
		name, err = ops.name()
		if err != nil || ops.event("child_mkdir") != nil {
			return nil, errStorage
		}
		err = parent.Mkdir(name, 0700)
		if err == nil {
			created = true
			break
		}
		if !os.IsExist(err) {
			return nil, errStorage
		}
	}
	if !created || ops.event("child_lstat") != nil {
		return nil, errStorage
	}
	createdInfo, err := parent.Lstat(name)
	if err != nil || !privateInfo(createdInfo, true) || ops.event("child_open") != nil {
		return nil, errStorage
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, errStorage
	}
	// Never close parent, including every failure path.
	s := &privateStore{root: root, directory: name, ops: ops}
	fail := func() (*privateStore, error) { _ = root.Close(); return nil, errStorage }
	if ops.event("child_identity") != nil {
		return fail()
	}
	dir, err := root.Open(".")
	if err != nil {
		return fail()
	}
	pinned, statErr := dir.Stat()
	closeErr = dir.Close()
	current, entryErr := parent.Lstat(name)
	if statErr != nil || closeErr != nil || entryErr != nil || !privateInfo(pinned, true) || !privateInfo(current, true) || !os.SameFile(createdInfo, pinned) || !os.SameFile(pinned, current) {
		return fail()
	}
	s.directoryInfo = pinned
	return s, nil
}

func (s *privateStore) close() error {
	if s == nil {
		return errStorage
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStorage
	}
	s.closed = true
	if s.root.Close() != nil {
		return errStorage
	}
	return nil
}

func (s *privateStore) destination() error {
	if s.ops.event("destination") != nil {
		return errStorage
	}
	i, err := s.root.Lstat("state.json")
	if s.expected == nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errStorage
	}
	if err != nil || !privateInfo(i, false) || !os.SameFile(i, s.expected) {
		return errStorage
	}
	return nil
}

func (s *privateStore) readLocked() (validationState, error) {
	if s.closed {
		return validationState{}, errStorage
	}
	if s.uncertain {
		return validationState{}, errUncertain
	}
	if s.expected == nil || s.destination() != nil {
		return validationState{}, errStorage
	}
	f, err := s.root.Open("state.json")
	if err != nil {
		return validationState{}, errStorage
	}
	i, statErr := f.Stat()
	if statErr != nil || !privateInfo(i, false) || !os.SameFile(i, s.expected) {
		_ = f.Close()
		return validationState{}, errStorage
	}
	state, readErr := parseState(f)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || state != s.last {
		return validationState{}, errStorage
	}
	return state, nil
}

func (s *privateStore) read() (validationState, error) {
	if s == nil {
		return validationState{}, errStorage
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked()
}

func (s *privateStore) cleanTemporary(name string, expected os.FileInfo) bool {
	if expected == nil {
		return false
	}
	i, err := s.root.Lstat(name)
	if os.IsNotExist(err) {
		return true
	}
	if err == nil && privateInfo(i, false) && os.SameFile(i, expected) {
		return s.ops.event("temporary_remove") == nil && s.root.Remove(name) == nil
	}
	return false
}

// publish never writes an existing inode. Rename is entry replacement, not a
// symlink/hardlink-target write. Same-UID tampering/mounts are not prevented.
func (s *privateStore) publish(next validationState, descriptor manifest, receipt *archiveReceipt) (bool, error) {
	if s == nil {
		return false, errStorage
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, errStorage
	}
	if s.uncertain {
		return false, errUncertain
	}
	if s.cleanupPending {
		return false, errStorage
	}
	if !next.valid() {
		return false, errState
	}
	if s.expected == nil {
		initial, err := newState(next.OperationID, descriptor)
		if err != nil || next != initial || receipt != nil {
			return false, errState
		}
	} else {
		previous, err := s.readLocked()
		if err != nil {
			return false, err
		}
		lawful, err := advanceState(previous, next.Status, descriptor, receipt, next.ErrorCode)
		if err != nil || lawful != next || !lawfulPublication(previous, next) {
			return false, errState
		}
	}
	if s.destination() != nil {
		return false, errStorage
	}
	directory, err := s.root.Open(".")
	if err != nil {
		return false, errStorage
	}
	dirInfo, statErr := directory.Stat()
	closeErr := directory.Close()
	if statErr != nil || closeErr != nil || !privateInfo(dirInfo, true) || !os.SameFile(dirInfo, s.directoryInfo) {
		return false, errStorage
	}
	b, err := next.canonical()
	if err != nil {
		return false, errState
	}
	var name string
	var f *os.File
	for attempt := 0; attempt < 8; attempt++ {
		name, err = s.ops.name()
		if err != nil || s.ops.event("file_open") != nil {
			return false, errStorage
		}
		f, err = s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return false, errStorage
		}
	}
	if err != nil || f == nil {
		return false, errStorage
	}
	if s.ops.event("file_identity") != nil {
		_ = f.Close()
		s.cleanupPending = true
		return false, errStorage
	}
	expected, statErr := f.Stat()
	if statErr != nil || !privateInfo(expected, false) {
		_ = f.Close()
		if !s.cleanTemporary(name, expected) {
			s.cleanupPending = true
		}
		return false, errStorage
	}
	renamed := false
	defer func() {
		if !renamed {
			// Prior confirmed state remains readable, but do not accumulate more
			// retained temporary files when exact cleanup cannot be confirmed.
			if !s.cleanTemporary(name, expected) {
				s.cleanupPending = true
			}
		}
	}()
	if s.ops.event("file_write") != nil {
		_ = f.Close()
		return false, errStorage
	}
	n, writeErr := s.ops.writeFile(f, b)
	if writeErr != nil || n != len(b) {
		_ = f.Close()
		return false, errStorage
	}
	if s.ops.event("file_sync") != nil {
		_ = f.Close()
		return false, errStorage
	}
	if s.ops.syncFile(f) != nil {
		_ = f.Close()
		return false, errStorage
	}
	hookErr := s.ops.event("file_close")
	closeErr = s.ops.closeFile(f)
	if hookErr != nil || closeErr != nil {
		return false, errStorage
	}
	if s.destination() != nil || s.ops.event("rename") != nil {
		return false, errStorage
	}
	if s.ops.rename(s.root, name, "state.json") != nil {
		return false, errStorage
	}
	renamed = true
	// Every subsequent failure is uncertain, including inability to observe.
	s.expected, s.observed, s.last, s.uncertain = expected, nil, next, true
	if s.ops.event("post_rename_identity") != nil {
		return false, errUncertain
	}
	observed, err := s.root.Lstat("state.json")
	if err != nil {
		return false, errUncertain
	}
	s.observed = observed
	if !privateInfo(observed, false) || !os.SameFile(expected, observed) {
		return false, errUncertain
	}
	if s.ops.event("directory_open") != nil {
		return false, errUncertain
	}
	directory, err = s.root.Open(".")
	if err != nil {
		return false, errUncertain
	}
	dirInfo, statErr = directory.Stat()
	if statErr != nil || !privateInfo(dirInfo, true) || !os.SameFile(dirInfo, s.directoryInfo) {
		_ = directory.Close()
		return false, errUncertain
	}
	hookErr = s.ops.event("directory_sync")
	var syncErr error
	if hookErr == nil {
		syncErr = s.ops.syncDirectory(directory)
	}
	closeHook := s.ops.event("directory_close")
	closeErr = s.ops.closeDirectory(directory)
	if hookErr != nil || syncErr != nil || closeHook != nil || closeErr != nil {
		return false, errUncertain
	}
	s.uncertain = false
	return true, nil
}
