package cobrowserecovery

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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

func (s FileStore) Begin() (Transaction, error) {
	if !Supported() {
		return nil, ErrUnavailable
	}
	created, err := privateDir(s.Dir)
	if err != nil {
		return nil, ErrUnavailable
	}
	lock, err := privateOpen(filepath.Join(s.Dir, "journal.lock"), true)
	if err != nil {
		return nil, ErrUnavailable
	}
	if err = lockExclusive(lock); err != nil {
		_ = lock.Close()
		return nil, ErrUnavailable
	}
	t := &fileTx{records: map[string]Record{}, dir: s.Dir, lock: lock, sync: s.sync}
	if t.sync == nil {
		t.sync = (*os.File).Sync
	}
	f, err := privateOpen(filepath.Join(s.Dir, "records.json"), false)
	if os.IsNotExist(err) {
		// Only the process that created a fresh directory may initialize an empty
		// snapshot. Missing state in an existing directory is a continuity failure,
		// not evidence that no earlier execution occurred.
		if !created {
			t.Close()
			return nil, ErrUnavailable
		}
		savedSync := t.sync
		t.sync = (*os.File).Sync
		initErr := t.Save()
		t.sync = savedSync
		if initErr != nil {
			t.Close()
			return nil, ErrUnavailable
		}
		return t, nil
	}
	if err != nil {
		t.Close()
		return nil, ErrUnavailable
	}
	b, err := io.ReadAll(io.LimitReader(f, maxJournalBytes+1))
	_ = f.Close()
	if err != nil || len(b) > maxJournalBytes {
		t.Close()
		return nil, ErrUnavailable
	}
	// Stream the bounded array so a small corrupt file containing millions of
	// empty records cannot allocate an unbounded []Record before the count check.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	opening, err := dec.Token()
	if err != nil || opening != json.Delim('[') {
		t.Close()
		return nil, ErrUnavailable
	}
	for dec.More() {
		if len(t.records) >= MaxRecords {
			t.Close()
			return nil, ErrUnavailable
		}
		var r Record
		if dec.Decode(&r) != nil || !validRecord(r) {
			t.Close()
			return nil, ErrUnavailable
		}
		if _, exists := t.records[r.JobID]; exists {
			t.Close()
			return nil, ErrUnavailable
		}
		t.records[r.JobID] = r
	}
	closing, err := dec.Token()
	if err != nil || closing != json.Delim(']') {
		t.Close()
		return nil, ErrUnavailable
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		t.Close()
		return nil, ErrUnavailable
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
