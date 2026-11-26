package laneop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Ledger statuses. StatusPending is written before any paste and reads back as
// submission_unknown, so a crash between paste and outcome never resubmits.
const (
	StatusSubmitted = "submitted"
	StatusUnknown   = "submission_unknown"
	StatusRejected  = "rejected"
	statusPending   = "pending"
)

const (
	// LedgerFileName and ledgerLockName live in the machine-level node
	// configuration directory chosen by cmd.
	LedgerFileName = "lane-offers.json"
	ledgerLockName = "lane-offers.lock"
	// DefaultLedgerTTL keeps settled offers well past the direct-Redis
	// reclaim floor and its DLQ cutoff, so a redelivered job still finds its
	// record. The server's own receipt keeps the offer for 30 days.
	DefaultLedgerTTL = 72 * time.Hour
	// DefaultLedgerCap bounds the file. A full ledger of unexpired records
	// refuses new sends rather than evicting one that a redelivery may need.
	DefaultLedgerCap = 4096
	maxLedgerBytes   = 4 << 20
)

var (
	// ErrLedgerFull means every slot holds an unexpired record.
	ErrLedgerFull = errors.New("lane offer ledger is full")
	// ErrLedgerUnavailable means the record could not be read or written.
	ErrLedgerUnavailable = errors.New("lane offer ledger is unavailable")
)

// Entry is one offer's record. Fingerprint covers the offer, lane, lease,
// node, pane and a SHA-256 of the message; the text itself is never stored.
type Entry struct {
	Fingerprint string `json:"fp"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	At          int64  `json:"at"`
}

// Ledger is the node-local, crash-durable once-per-offer record. Every
// operation holds a process mutex and an OS file lock, and every write is an
// fsynced atomic replace.
type Ledger struct {
	dir string
	ttl time.Duration
	cap int
	mu  sync.Mutex
}

// NewLedger roots a ledger in dir. It returns nil unless dir is absolute, so
// a node without a configuration directory never advertises sends.
func NewLedger(dir string) *Ledger {
	if dir == "" || !filepath.IsAbs(dir) {
		return nil
	}
	return &Ledger{dir: dir, ttl: DefaultLedgerTTL, cap: DefaultLedgerCap}
}

type ledgerFile struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

// Lookup returns the offer's record, if any.
func (l *Ledger) Lookup(offerID string) (Entry, bool, error) {
	var entry Entry
	var found bool
	err := l.locked(func(f *ledgerFile) (bool, error) {
		entry, found = f.Entries[offerID]
		return false, nil
	})
	return entry, found, err
}

// Reserve records an offer that has no record yet. If one exists it is
// returned unchanged with existed=true and nothing is written.
func (l *Ledger) Reserve(offerID string, entry Entry, now time.Time) (Entry, bool, error) {
	var existing Entry
	var existed bool
	err := l.locked(func(f *ledgerFile) (bool, error) {
		if prior, ok := f.Entries[offerID]; ok {
			existing, existed = prior, true
			return false, nil
		}
		l.prune(f, now)
		if len(f.Entries) >= l.cap {
			return false, ErrLedgerFull
		}
		entry.At = now.Unix()
		f.Entries[offerID] = entry
		return true, nil
	})
	return existing, existed, err
}

// Settle replaces a pending record with its outcome. A record whose
// fingerprint differs is left alone.
func (l *Ledger) Settle(offerID, fingerprint, status, reason string, now time.Time) error {
	return l.locked(func(f *ledgerFile) (bool, error) {
		prior, ok := f.Entries[offerID]
		if !ok || prior.Fingerprint != fingerprint {
			return false, ErrLedgerUnavailable
		}
		f.Entries[offerID] = Entry{Fingerprint: fingerprint, Status: status, Reason: reason, At: now.Unix()}
		return true, nil
	})
}

// prune drops settled records older than the TTL. A pending record is never
// dropped: it is the only proof that a paste may have happened.
func (l *Ledger) prune(f *ledgerFile, now time.Time) {
	cutoff := now.Add(-l.ttl).Unix()
	for id, entry := range f.Entries {
		if entry.Status != statusPending && entry.At < cutoff {
			delete(f.Entries, id)
		}
	}
}

func (l *Ledger) locked(fn func(*ledgerFile) (bool, error)) error {
	if l == nil || l.dir == "" || !filepath.IsAbs(l.dir) {
		return ErrLedgerUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return ErrLedgerUnavailable
	}
	unlock, err := lockFile(filepath.Join(l.dir, ledgerLockName))
	if err != nil {
		return ErrLedgerUnavailable
	}
	defer unlock()
	path := filepath.Join(l.dir, LedgerFileName)
	file := ledgerFile{Version: 1, Entries: map[string]Entry{}}
	raw, err := readBounded(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return ErrLedgerUnavailable
	default:
		// A damaged ledger is never rewritten from empty: that would forget
		// which offers may already have been submitted.
		if json.Unmarshal(raw, &file) != nil || file.Version != 1 || file.Entries == nil {
			return ErrLedgerUnavailable
		}
	}
	changed, err := fn(&file)
	if err != nil || !changed {
		return err
	}
	encoded, err := json.Marshal(file)
	if err != nil || len(encoded) > maxLedgerBytes {
		return ErrLedgerUnavailable
	}
	if err := writeAtomic(path, encoded); err != nil {
		return ErrLedgerUnavailable
	}
	return nil
}

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxLedgerBytes {
		return nil, fmt.Errorf("lane offer ledger is not a bounded regular file")
	}
	buf := make([]byte, info.Size())
	if _, err := f.ReadAt(buf, 0); err != nil && info.Size() > 0 {
		return nil, err
	}
	return buf, nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lane-offers-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
