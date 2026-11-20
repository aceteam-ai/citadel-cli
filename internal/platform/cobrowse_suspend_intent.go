package platform

// Unwired suspend-intent persistence, using the existing lifecycle registry.
// The caller must hold the session lifecycle exclusion across capture, staging
// and teardown. This store's lock excludes other suspend-store instances, NOT
// legacy live-record writers. There is deliberately no manager/job caller yet.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
)

const maxSuspendRegistryRecordBytes = 64 * 1024
const maxSuspendRegistryBytes = 8 * 1024 * 1024
const maxSuspendRegistryEntries = 1024

type suspendIntentStore struct {
	mu           sync.Mutex
	root         *os.Root
	payloads     *suspendManifestStore
	syncFile     func(*os.File) error
	syncDir      func() error
	rename       func(string, string) error
	removeRecord func(string) error
}

func openSuspendIntentStore(regDir string) (*suspendIntentStore, error) {
	payloads, err := openSuspendManifestStore(regDir, true)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(regDir)
	if err != nil {
		_ = payloads.close()
		return nil, errors.New("open browser lifecycle registry")
	}
	s := &suspendIntentStore{root: root, payloads: payloads, syncFile: func(f *os.File) error { return f.Sync() }, rename: root.Rename, removeRecord: root.Remove}
	s.syncDir = func() error {
		if runtime.GOOS == "windows" {
			// File flush plus atomic replacement covers process restart, not
			// machine power loss, consistently with the payload store.
			return nil
		}
		f, err := root.Open(".")
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Sync()
	}
	if s.private() != nil {
		_ = s.close()
		return nil, errors.New("browser lifecycle registry is not private")
	}
	return s, nil
}

func (s *suspendIntentStore) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.root.Close(), s.payloads.close())
}

func (s *suspendIntentStore) private() error {
	f, err := s.root.Open(".")
	if err != nil {
		return errors.New("private browser lifecycle registry unavailable")
	}
	info, err := f.Stat()
	_ = f.Close()
	if err != nil || validateCobrowseBaseDirPerms(s.root.Name(), info) != nil ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return errors.New("browser lifecycle registry is no longer private")
	}
	// Both roots must still describe the named registry/payload pair. A
	// detached directory must never receive an acknowledged manifest write.
	named, err := s.root.Lstat(suspendManifestDirName)
	if err != nil || !named.IsDir() {
		return errors.New("browser payload directory changed")
	}
	payloadDir, err := s.payloads.root.Open(".")
	if err != nil {
		return errors.New("browser payload directory unavailable")
	}
	opened, err := payloadDir.Stat()
	_ = payloadDir.Close()
	if err != nil || !os.SameFile(named, opened) {
		return errors.New("browser payload directory changed")
	}
	return nil
}

func (s *suspendIntentStore) read(id string) (sessionRecord, error) {
	var rec sessionRecord
	if !sessionIDPattern.MatchString(id) || len(id) > 64 || s.private() != nil {
		return rec, errors.New("invalid private browser lifecycle record")
	}
	name := id + ".json"
	info, err := s.root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxSuspendRegistryRecordBytes ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return rec, errors.New("invalid private browser lifecycle record")
	}
	f, err := s.root.Open(name)
	if err != nil {
		return rec, errors.New("open private browser lifecycle record")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return rec, errors.New("browser lifecycle record changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSuspendRegistryRecordBytes+1))
	if err != nil || len(data) > maxSuspendRegistryRecordBytes || json.Unmarshal(data, &rec) != nil || rec.ID != id || rec.Version != sessionRecordVersion {
		return sessionRecord{}, errors.New("invalid browser lifecycle record contents")
	}
	return rec, nil
}

// Unlike legacy listRecords, this bounded accounting scan never skips a bad
// record: doing so could turn another owner's retained manifest into capacity.
// Reserve one entry and one maximum-sized record for the atomic replacement
// temp file, so the write itself cannot exceed the physical registry budget.
func (s *suspendIntentStore) snapshot(ctx context.Context) ([]sessionRecord, error) {
	f, err := s.root.Open(".")
	if err != nil {
		return nil, errors.New("enumerate browser lifecycle registry")
	}
	defer f.Close()
	var records []sessionRecord
	count, used := 0, int64(0)
	for {
		entries, err := f.ReadDir(64)
		for _, entry := range entries {
			count++
			if ctx.Err() != nil || count >= maxSuspendRegistryEntries {
				return nil, errors.New("browser lifecycle scan exceeded its bound")
			}
			if entry.Name() == suspendManifestDirName {
				continue
			}
			info, statErr := s.root.Lstat(entry.Name())
			if statErr != nil || !info.Mode().IsRegular() || info.Size() < 0 ||
				info.Size() > maxSuspendRegistryRecordBytes || used > maxSuspendRegistryBytes-maxSuspendRegistryRecordBytes-info.Size() {
				return nil, errors.New("cannot establish browser lifecycle capacity")
			}
			used += info.Size()
			if strings.HasPrefix(entry.Name(), ".tmp-") {
				continue // bounded crash leftovers, never delete them here
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			if id == entry.Name() {
				return nil, errors.New("unrecognized browser lifecycle entry")
			}
			rec, readErr := s.read(id)
			if readErr != nil {
				return nil, readErr
			}
			switch rec.State {
			case RecordLive, RecordExpired:
			case RecordSuspended, "suspending", "resuming":
				if _, err := suspendExpiry(rec); err != nil {
					return nil, errors.New("invalid retained browser lifecycle accounting")
				}
			default:
				return nil, errors.New("unknown browser lifecycle state")
			}
			records = append(records, rec)
		}
		if errors.Is(err, io.EOF) {
			return records, nil
		}
		if err != nil {
			return nil, errors.New("enumerate browser lifecycle registry")
		}
	}
}

// stage keeps the original live-process identity in the suspending intent for
// crash recovery; the final suspended record clears it only AFTER teardown.
// Failed or uncertain commits leave the payload charged, never delete a file
// which a durable intent may already reference. No browser effect happens here.
func (s *suspendIntentStore) stage(ctx context.Context, expected sessionRecord, owner string,
	capture preparedSuspendCapture, now time.Time, fence func() error) (sessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func() (sessionRecord, error) {
		return sessionRecord{}, errors.New("could not persist browser suspend intent")
	}
	check := func() bool { return ctx.Err() == nil && fence != nil && fence() == nil && s.private() == nil }
	if _, ok := ctx.Deadline(); !ok || !check() {
		return fail()
	}
	current, err := s.read(expected.ID)
	if err != nil || !reflect.DeepEqual(current, expected) {
		return fail()
	}
	retained, err := s.snapshot(ctx)
	if err != nil || !check() {
		return fail()
	}
	final, err := planSuspendedRecord(expected, owner, capture.profileName, capture.tabs, int64(len(capture.data)), retained, now)
	if err != nil || s.payloads.write(final, capture) != nil || !check() {
		return fail()
	}
	intent := expected
	intent.State, intent.Suspension = "suspending", final.Suspension
	intent.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	if err := s.replaceRecord(expected, intent, check); err != nil {
		return fail()
	}
	return intent, nil
}

// replaceRecord runs under mu and the caller's lifecycle exclusion. It does
// not allocate capacity: callers reserve an atomic replacement slot via
// snapshot before reaching it. Shared by staging and TTL transitions, with
// the same file-flush, compare, atomic-replace and directory-flush ordering.
func (s *suspendIntentStore) replaceRecord(expected, next sessionRecord, check func() bool) error {
	fail := func() error { return errors.New("could not persist browser lifecycle transition") }
	if next.ID != expected.ID || next.Version != sessionRecordVersion || check == nil || !check() {
		return fail()
	}
	data, err := json.Marshal(next)
	if err != nil || len(data) > maxSuspendRegistryRecordBytes {
		return fail()
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fail()
	}
	name := ".tmp-" + hex.EncodeToString(nonce[:])
	f, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fail()
	}
	defer func() { _ = s.root.Remove(name) }()
	_, err = f.Write(data)
	if err == nil {
		err = s.syncFile(f)
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil || !check() {
		return fail()
	}
	current, err := s.read(expected.ID)
	if err != nil || !reflect.DeepEqual(current, expected) || !check() || s.rename(name, expected.ID+".json") != nil || s.syncDir() != nil || !check() {
		return fail()
	}
	return nil
}
