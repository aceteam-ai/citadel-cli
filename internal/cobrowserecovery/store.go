package cobrowserecovery

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

const maxJournalBytes = 8 << 20

// FileStore is a single bounded snapshot protected by an OS advisory lock. Both
// the directory and every opened file must already have private, no-link modes.
// Windows refuses until a protected-DACL/durable-publication adapter exists.
type FileStore struct {
	Dir  string
	sync func(*os.File) error
}
type fileTx struct {
	records map[string]Record
	dir     string
	lock    *os.File
	sync    func(*os.File) error
}

func (t *fileTx) Records() map[string]Record { return t.records }
func (t *fileTx) Close()                     { _ = t.lock.Close() }

// readIdentity reads a private, non-secret incarnation marker. It is storage
// continuity evidence only, never a credential or authority to launch.
func readIdentity(path string) (string, error) {
	f, err := privateOpen(path, false)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 37))
	if err != nil || len(b) != 36 || !canonicalID(string(b)) {
		return "", ErrUnavailable
	}
	return string(b), nil
}
func createIdentity(path, value string) error {
	// Exclusive creation refuses links and never overwrites an earlier marker.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrUnavailable
	}
	if _, err = f.WriteString(value); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrUnavailable
	}
	return nil
}
func syncParent(path string) error {
	f, err := os.Open(filepath.Dir(path))
	if err != nil {
		return ErrUnavailable
	}
	defer f.Close()
	return f.Sync()
}
func (s FileStore) Begin() (Transaction, error) {
	if !Supported() {
		return nil, ErrUnavailable
	}
	// The lock and immutable anchor live OUTSIDE the journal directory. Moving
	// that directory cannot create another custody lock or a fresh empty ledger.
	lock, err := privateOpen(s.Dir+".lock", true)
	if err != nil {
		return nil, ErrUnavailable
	}
	if lockExclusive(lock) != nil {
		_ = lock.Close()
		return nil, ErrUnavailable
	}
	t := &fileTx{records: map[string]Record{}, dir: s.Dir, lock: lock, sync: (*os.File).Sync}
	refuse := func() (Transaction, error) { t.Close(); return nil, ErrUnavailable }
	anchor, err := readIdentity(s.Dir + ".anchor")
	if os.IsNotExist(err) {
		// Initialization is allowed only when BOTH anchor and directory are absent.
		// A partly initialized or legacy directory needs explicit reconciliation.
		if _, statErr := os.Lstat(s.Dir); !os.IsNotExist(statErr) {
			return refuse()
		}
		created, dirErr := privateDir(s.Dir)
		if dirErr != nil || !created {
			return refuse()
		}
		generation, genErr := uuid.NewRandom()
		if genErr != nil {
			return refuse()
		}
		marker := generation.String()
		if createIdentity(filepath.Join(s.Dir, "identity"), marker) != nil || t.Save() != nil {
			return refuse()
		}
		if createIdentity(s.Dir+".anchor", marker) != nil || syncParent(s.Dir) != nil {
			return refuse()
		}
	} else {
		if err != nil {
			return refuse()
		}
		// Never recreate a missing directory when an earlier incarnation exists.
		if err = checkPrivateDir(s.Dir); err != nil {
			return refuse()
		}
		marker, markerErr := readIdentity(filepath.Join(s.Dir, "identity"))
		if markerErr != nil || marker != anchor {
			return refuse()
		}
	}
	if s.sync != nil {
		t.sync = s.sync
	}
	f, err := privateOpen(filepath.Join(s.Dir, "records.json"), false)
	if err != nil {
		return refuse()
	}
	b, err := io.ReadAll(io.LimitReader(f, maxJournalBytes+1))
	_ = f.Close()
	if err != nil || len(b) > maxJournalBytes {
		return refuse()
	}
	// Decode incrementally: cap records before allocating another entry.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	opening, err := dec.Token()
	if err != nil || opening != json.Delim('[') {
		return refuse()
	}
	for dec.More() {
		if len(t.records) >= MaxRecords {
			return refuse()
		}
		var r Record
		if dec.Decode(&r) != nil || !validRecord(r) {
			return refuse()
		}
		if _, exists := t.records[r.JobID]; exists {
			return refuse()
		}
		t.records[r.JobID] = r
	}
	closing, err := dec.Token()
	if err != nil || closing != json.Delim(']') {
		return refuse()
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return refuse()
	}
	return t, nil
}

func (t *fileTx) Save() error {
	if len(t.records) > MaxRecords {
		return ErrUnavailable
	}
	rows := make([]Record, 0, len(t.records))
	for _, r := range t.records {
		if !validRecord(r) {
			return ErrUnavailable
		}
		rows = append(rows, r)
	}
	b, err := json.Marshal(rows)
	if err != nil || len(b) > maxJournalBytes {
		return ErrUnavailable
	}
	// A single reserved staging name bounds crash leftovers. The OS lock is
	// already held; no-follow, ownership and link checks precede truncation.
	next := filepath.Join(t.dir, "records.next")
	f, err := privateOpen(next, true)
	if err != nil {
		return ErrUnavailable
	}
	defer os.Remove(next)
	if f.Truncate(0) != nil {
		_ = f.Close()
		return ErrUnavailable
	}
	if _, err = f.Write(b); err == nil {
		err = t.sync(f)
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrUnavailable
	}
	if os.Rename(next, filepath.Join(t.dir, "records.json")) != nil {
		return ErrUnavailable
	}
	dir, err := os.Open(t.dir)
	if err != nil {
		return ErrUnavailable
	}
	defer dir.Close()
	if t.sync(dir) != nil {
		return ErrUnavailable
	}
	return nil
}
