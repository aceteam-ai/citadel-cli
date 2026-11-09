// Package localaudit is Citadel local mode's co-browse audit log (aceteam#13503
// design section 2.6): append-only JSONL under <node config dir>/cobrowse-audit,
// one file per UTC month, each record carrying the SHA-256 of the record before
// it so an edited, removed or reordered record is detectable.
//
// The chain detects tampering after the fact. It is not proof against a hostile
// process running as the same OS user, which could rewrite the whole chain.
//
// Typed text, passphrases and PINs are never recorded: the caller passes only a
// text length, and URLs are stored without query string or fragment.
package localaudit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DirName is the audit directory under the node config dir.
const DirName = "cobrowse-audit"

// DefaultRetention is how long monthly files are kept.
const DefaultRetention = 90 * 24 * time.Hour

// Record is one audited call or human action.
type Record struct {
	V         int    `json:"v"`
	Seq       int64  `json:"seq"`
	CreatedAt string `json:"created_at"`
	Source    string `json:"source"`
	SessionID string `json:"session_id,omitempty"`
	// Actor is "agent" for MCP tool calls, "user" for viewer actions.
	Actor string `json:"actor"`
	Agent string `json:"agent,omitempty"`
	// IntendedAction is the requested action (navigate, click, approve, ...).
	IntendedAction string `json:"intended_action"`
	Class          string `json:"class"`
	Goal           string `json:"goal,omitempty"`
	URL            string `json:"url,omitempty"`
	Selector       string `json:"selector,omitempty"`
	TextLen        int    `json:"text_len,omitempty"`
	// ResultingAction is the outcome: ok, refused, error, or a lifecycle note.
	ResultingAction string `json:"resulting_action"`
	Error           string `json:"error,omitempty"`
	PrevHash        string `json:"prev_hash"`
	Hash            string `json:"hash"`
}

// hashRecord is the SHA-256 of the record's JSON with Hash empty. Field order
// is the struct's, so the encoding is deterministic.
func hashRecord(r Record) string {
	r.Hash = ""
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SanitizeURL drops userinfo, query and fragment, which can carry tokens.
func SanitizeURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		if len(raw) > 200 {
			raw = raw[:200]
		}
		return strings.SplitN(strings.SplitN(raw, "?", 2)[0], "#", 2)[0]
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// Log is an open audit log.
type Log struct {
	dir       string
	retention time.Duration
	now       func() time.Time

	mu   sync.Mutex
	seq  int64
	prev string
}

// Open opens (creating) the audit dir, applies retention, and resumes the
// chain from the newest record. A log whose newest file cannot be read is an
// error: appending to a chain that cannot be checked would break it silently.
func Open(dir string, retention time.Duration, now func() time.Time) (*Log, error) {
	if retention <= 0 {
		retention = DefaultRetention
	}
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create audit dir %s: %w", dir, err)
	}
	l := &Log{dir: dir, retention: retention, now: now}
	// Resume the chain head BEFORE retention, so pruning every old month never
	// restarts the sequence or orphans the next record's link.
	files, err := monthFiles(dir)
	if err != nil {
		return nil, err
	}
	for i := len(files) - 1; i >= 0; i-- {
		last, ok, err := lastRecord(files[i])
		if err != nil {
			return nil, fmt.Errorf("read audit log %s: %w", files[i], err)
		}
		if ok {
			l.seq, l.prev = last.Seq, last.Hash
			break
		}
	}
	if err := l.prune(); err != nil {
		return nil, err
	}
	return l, nil
}

// Dir is the audit directory.
func (l *Log) Dir() string { return l.dir }

// Append fills in the chain fields and writes r durably.
func (l *Log) Append(r Record) (Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().UTC()
	r.V = 1
	r.Source = "local"
	r.Seq = l.seq + 1
	r.CreatedAt = now.Format(time.RFC3339Nano)
	r.URL = SanitizeURL(r.URL)
	r.PrevHash = l.prev
	r.Hash = hashRecord(r)
	line, err := json.Marshal(r)
	if err != nil {
		return Record{}, err
	}
	path := filepath.Join(l.dir, now.Format("2006-01")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Record{}, fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return Record{}, fmt.Errorf("write audit log: %w", err)
	}
	if err := f.Sync(); err != nil {
		return Record{}, fmt.Errorf("sync audit log: %w", err)
	}
	l.seq, l.prev = r.Seq, r.Hash
	return r, nil
}

// prune removes monthly files whose month ended more than retention ago.
func (l *Log) prune() error {
	files, err := monthFiles(l.dir)
	if err != nil {
		return err
	}
	cutoff := l.now().UTC().Add(-l.retention)
	for _, f := range files {
		month, err := time.Parse("2006-01", strings.TrimSuffix(filepath.Base(f), ".jsonl"))
		if err != nil {
			continue
		}
		if month.AddDate(0, 1, 0).Before(cutoff) {
			if err := os.Remove(f); err != nil {
				return fmt.Errorf("apply audit retention: %w", err)
			}
		}
	}
	return nil
}

// monthFiles lists YYYY-MM.jsonl files oldest first.
func monthFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if _, err := time.Parse("2006-01", strings.TrimSuffix(name, ".jsonl")); err != nil {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out, nil
}

func eachRecord(path string, fn func(line int, r Record, raw []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
		raw := sc.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("%s:%d: not a valid record: %w", filepath.Base(path), n, err)
		}
		if err := fn(n, r, raw); err != nil {
			return err
		}
	}
	return sc.Err()
}

func lastRecord(path string) (Record, bool, error) {
	var last Record
	ok := false
	err := eachRecord(path, func(_ int, r Record, _ []byte) error {
		last, ok = r, true
		return nil
	})
	return last, ok, err
}

// VerifyResult summarizes a chain check.
type VerifyResult struct {
	Records  int
	FirstSeq int64
	LastSeq  int64
	// Truncated is true when the oldest retained record is not seq 1, i.e.
	// earlier months were removed by retention.
	Truncated bool
}

// Verify recomputes every hash and link. It returns the first break found,
// naming the file and line.
func Verify(dir string) (VerifyResult, error) {
	var res VerifyResult
	files, err := monthFiles(dir)
	if err != nil {
		return res, err
	}
	prev := ""
	for _, f := range files {
		err := eachRecord(f, func(line int, r Record, _ []byte) error {
			where := fmt.Sprintf("%s:%d (seq %d)", filepath.Base(f), line, r.Seq)
			if got := hashRecord(r); got != r.Hash {
				return fmt.Errorf("%s: record was modified (hash mismatch)", where)
			}
			if res.Records == 0 {
				res.FirstSeq = r.Seq
				res.Truncated = r.Seq != 1
				if r.Seq == 1 && r.PrevHash != "" {
					return fmt.Errorf("%s: first record links to a missing predecessor", where)
				}
			} else {
				if r.PrevHash != prev {
					return fmt.Errorf("%s: chain broken (a record before it was removed, edited or reordered)", where)
				}
				if r.Seq != res.LastSeq+1 {
					return fmt.Errorf("%s: sequence jumps from %d", where, res.LastSeq)
				}
			}
			prev = r.Hash
			res.LastSeq = r.Seq
			res.Records++
			return nil
		})
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// Filter selects records for Read.
type Filter struct {
	SessionID string
	Since     time.Time
	// Limit keeps the newest N records; 0 means all.
	Limit int
}

// Read returns matching records oldest first.
func Read(dir string, f Filter) ([]Record, error) {
	files, err := monthFiles(dir)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, path := range files {
		err := eachRecord(path, func(_ int, r Record, _ []byte) error {
			if f.SessionID != "" && r.SessionID != f.SessionID {
				return nil
			}
			if !f.Since.IsZero() {
				if t, err := time.Parse(time.RFC3339Nano, r.CreatedAt); err == nil && t.Before(f.Since) {
					return nil
				}
			}
			out = append(out, r)
			if f.Limit > 0 && len(out) > 4*f.Limit {
				out = append([]Record(nil), out[len(out)-f.Limit:]...)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out, nil
}

// WriteText prints records as an aligned table.
func WriteText(w io.Writer, recs []Record) {
	for _, r := range recs {
		t := r.CreatedAt
		if ts, err := time.Parse(time.RFC3339Nano, r.CreatedAt); err == nil {
			t = ts.Local().Format("2006-01-02 15:04:05")
		}
		detail := r.URL
		if detail == "" {
			detail = r.Selector
		}
		if r.TextLen > 0 {
			detail = fmt.Sprintf("%d characters", r.TextLen)
		}
		if r.Error != "" {
			detail = strings.TrimSpace(detail + " " + r.Error)
		}
		who := r.Actor
		if r.Agent != "" && r.Actor == "agent" {
			who = r.Agent
		}
		fmt.Fprintf(w, "%s  %-12s %-6s %-14s %-9s %s\n", t, r.SessionID, truncate(who, 6), r.IntendedAction, r.ResultingAction, detail)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
