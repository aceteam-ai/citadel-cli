package platform

// Private manifest payloads, not a second lifecycle registry. No production
// caller exists yet. The manager must serialize lifecycle transitions, authorize
// the owner and commit the registry only AFTER this store acknowledges a write.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
)

const suspendManifestDirName = "suspended-tabs"
const maxSuspendStoreEntries = 1024
const suspendStoreLockName = ".store.lock"

type suspendManifestSealer interface {
	SuspendProfileName() string
	SealSuspendManifest(string, uint64, []byte) ([]byte, error)
	UnsealSuspendManifest(string, uint64, []byte) ([]byte, error)
}

type preparedSuspendCapture struct {
	id          string
	generation  uint64
	persistent  bool
	profileName string
	tabs        int
	data        []byte
}

func (preparedSuspendCapture) String() string     { return "[private prepared browser manifest]" }
func (p preparedSuspendCapture) GoString() string { return p.String() }
func (p *preparedSuspendCapture) clear()          { clear(p.data); p.data = nil }

func prepareSuspendCapture(m suspendManifest, persistent bool, profileName string, sealer suspendManifestSealer) (preparedSuspendCapture, error) {
	if persistent != (profileName != "") || (persistent && (!suspendProfileName.MatchString(profileName) || sealer == nil || sealer.SuspendProfileName() != profileName)) {
		return preparedSuspendCapture{}, errors.New("browser capture requires the matching unlocked profile")
	}
	plain, err := encodeSuspendManifest(m)
	if err != nil {
		return preparedSuspendCapture{}, err
	}
	data := plain
	if persistent {
		defer clear(plain)
		if sealer == nil {
			return preparedSuspendCapture{}, errors.New("saved browser manifest requires an unlocked profile")
		}
		data, err = sealer.SealSuspendManifest(m.SessionID, m.Generation, plain)
		if err != nil {
			clear(data)
			return preparedSuspendCapture{}, errors.New("could not seal saved browser manifest")
		}
	}
	if len(data) == 0 || len(data) > maxSuspendManifestBytes+4096 {
		clear(data)
		return preparedSuspendCapture{}, errors.New("stored browser manifest exceeds byte bound")
	}
	return preparedSuspendCapture{id: m.SessionID, generation: m.Generation, persistent: persistent, profileName: profileName, tabs: len(m.Tabs), data: data}, nil
}

// One instance belongs to the node-wide manager. mu serializes its filesystem
// operations; the manager's lifecycle lock must additionally fence the registry
// transition and any old-worker handoff. Merely opening a store grants no owner
// or browser authority.
type suspendManifestStore struct {
	mu        sync.Mutex
	root      *os.Root
	lockFile  *os.File
	closeOnce sync.Once
	closeErr  error
	syncFile  func(*os.File) error
	syncDir   func() error
	publish   func(string, string) error
}

func openSuspendManifestStore(regDir string, create bool) (*suspendManifestStore, error) {
	dir := filepath.Join(regDir, suspendManifestDirName)
	if create {
		if err := ensureCobrowseBaseDir(regDir); err != nil {
			return nil, errors.New("private browser registry unavailable")
		}
		if err := ensureCobrowseBaseDir(dir); err != nil {
			return nil, errors.New("private browser manifest directory unavailable")
		}
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || validateCobrowseBaseDirPerms(dir, info) != nil ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("private browser manifest directory unavailable")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.New("open private browser manifest directory")
	}
	f, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, errors.New("verify private browser manifest directory")
	}
	opened, statErr := f.Stat()
	_ = f.Close()
	if statErr != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, errors.New("browser manifest directory changed")
	}
	lock, err := root.OpenFile(suspendStoreLockName, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = root.Close()
		return nil, errors.New("open browser manifest ownership lock")
	}
	lockInfo, e1 := root.Lstat(suspendStoreLockName)
	openedLock, e2 := lock.Stat()
	if e1 != nil || e2 != nil || !lockInfo.Mode().IsRegular() || !os.SameFile(lockInfo, openedLock) ||
		(runtime.GOOS != "windows" && lockInfo.Mode().Perm()&0o077 != 0) {
		_ = lock.Close()
		_ = root.Close()
		return nil, errors.New("invalid browser manifest ownership lock")
	}
	if err := lockSuspendManifestStore(lock); err != nil {
		_ = lock.Close()
		_ = root.Close()
		return nil, errors.New("browser manifest storage is busy; retry later")
	}
	s := &suspendManifestStore{root: root, lockFile: lock, syncFile: func(f *os.File) error { return f.Sync() }, publish: root.Link}
	s.syncDir = func() error {
		if runtime.GOOS == "windows" {
			return nil
		}
		d, err := root.Open(".")
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	}
	return s, nil
}

func (s *suspendManifestStore) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeOnce.Do(func() { s.closeErr = errors.Join(s.root.Close(), s.lockFile.Close()) })
	return s.closeErr
}

func (s *suspendManifestStore) private() error {
	f, err := s.root.Open(".")
	if err != nil {
		return errors.New("private browser manifest directory unavailable")
	}
	info, err := f.Stat()
	_ = f.Close()
	if err != nil || validateCobrowseBaseDirPerms(s.root.Name(), info) != nil ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return errors.New("browser manifest directory is no longer private")
	}
	return nil
}

func suspendManifestName(rec sessionRecord) (string, error) {
	if rec.State != RecordSuspended && rec.State != "suspending" && rec.State != "resuming" {
		return "", errors.New("browser record does not retain a manifest")
	}
	if _, err := suspendExpiry(rec); err != nil {
		return "", err
	}
	return rec.ID + "." + strconv.FormatUint(rec.Suspension.CaptureGeneration, 10) + ".tabs", nil
}

// usage includes unreferenced and temporary files: a crash must not turn their
// real disk usage into free capacity. Unknown file types, oversize files and an
// unbounded directory refuse allocation rather than silently undercounting.
func (s *suspendManifestStore) usage() (int64, error) {
	dir, err := s.root.Open(".")
	if err != nil {
		return 0, errors.New("cannot account for browser manifest storage")
	}
	defer dir.Close()
	var used int64
	count := 0
	for {
		entries, err := dir.ReadDir(64)
		for _, entry := range entries {
			count++
			if count >= maxSuspendStoreEntries {
				return 0, errors.New("browser manifest directory exceeds entry bound")
			}
			info, statErr := s.root.Lstat(entry.Name())
			if statErr != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxSuspendManifestBytes+4096 ||
				used > maxSuspendedNodeBytes-info.Size() {
				return 0, errors.New("cannot establish bounded browser manifest storage")
			}
			used += info.Size()
		}
		if errors.Is(err, io.EOF) {
			return used, nil
		}
		if err != nil {
			return 0, errors.New("cannot account for browser manifest storage")
		}
	}
}

// write is create-only. The payload is written and synced privately before an
// atomic hard-link publishes the final name, so a reader never sees a partial
// file and an existing generation (including a symlink) is never overwritten.
// Filesystems without hard links refuse safely; the browser must stay live.
func (s *suspendManifestStore) write(rec sessionRecord, capture preparedSuspendCapture) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.private(); err != nil {
		return err
	}
	name, err := suspendManifestName(rec)
	if err != nil {
		return err
	}
	if capture.id != rec.ID || capture.generation != rec.Generation || capture.persistent != rec.Persistent ||
		capture.profileName != rec.ProfileName ||
		capture.tabs != rec.Suspension.TabCount || len(capture.data) == 0 ||
		int64(len(capture.data)) != rec.Suspension.ManifestBytes {
		return errors.New("prepared browser capture does not match registry record")
	}
	used, err := s.usage()
	if err != nil {
		return err
	}
	if used > maxSuspendedNodeBytes-int64(len(capture.data)) {
		return errors.New("browser manifest storage capacity reached")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errors.New("create private browser manifest name")
	}
	tmp := ".tmp-" + hex.EncodeToString(nonce[:])
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create private browser manifest")
	}
	defer func() { _ = s.root.Remove(tmp) }()
	_, writeErr := f.Write(capture.data)
	if writeErr == nil {
		writeErr = s.syncFile(f)
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("persist private browser manifest")
	}
	if err := s.publish(tmp, name); err != nil {
		return errors.New("publish private browser manifest; existing capture was not replaced")
	}
	// Sync directory metadata where supported. On Windows the file flush and
	// atomic link protect process-restart recovery; no power-loss guarantee is
	// claimed by this store or by the existing registry writer.
	if err := s.syncDir(); err != nil {
		return errors.New("sync browser manifest directory")
	}
	return nil
}

func (s *suspendManifestStore) read(rec sessionRecord, sealer suspendManifestSealer) (suspendManifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.private(); err != nil {
		return suspendManifest{}, err
	}
	name, err := suspendManifestName(rec)
	if err != nil {
		return suspendManifest{}, err
	}
	info, err := s.root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() != rec.Suspension.ManifestBytes ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return suspendManifest{}, errors.New("private browser manifest missing or invalid")
	}
	f, err := s.root.Open(name)
	if err != nil {
		return suspendManifest{}, errors.New("open private browser manifest")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return suspendManifest{}, errors.New("browser manifest changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSuspendManifestBytes+4096+1))
	defer clear(data)
	if err != nil || int64(len(data)) != rec.Suspension.ManifestBytes {
		return suspendManifest{}, errors.New("read bounded browser manifest")
	}
	plain := data
	if rec.Persistent {
		if sealer == nil || sealer.SuspendProfileName() != rec.ProfileName {
			return suspendManifest{}, errors.New("human unlock is required to read saved browser tabs")
		}
		plain, err = sealer.UnsealSuspendManifest(rec.ID, rec.Suspension.CaptureGeneration, data)
		defer clear(plain)
		if err != nil {
			return suspendManifest{}, errors.New("could not unlock saved browser tabs")
		}
	}
	m, err := decodeSuspendManifest(plain, rec.ID, rec.Suspension.CaptureGeneration)
	if err != nil {
		return suspendManifest{}, err
	}
	if len(m.Tabs) != rec.Suspension.TabCount {
		return suspendManifest{}, errors.New("browser manifest tab count mismatch")
	}
	return m, nil
}

func (s *suspendManifestStore) remove(rec sessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, err := suspendManifestName(rec)
	if err != nil {
		return err
	}
	info, err := s.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		if err := s.syncDir(); err != nil {
			return errors.New("sync absent browser manifest")
		}
		return nil
	}
	if err != nil || (!info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0) {
		return errors.New("refusing to remove an unexpected browser manifest object")
	}
	if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("remove private browser manifest")
	}
	if err := s.syncDir(); err != nil {
		return errors.New("sync removed browser manifest")
	}
	return nil
}

func (s *suspendManifestStore) String() string { return "[private browser manifest store]" }
