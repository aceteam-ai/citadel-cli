// Package localaudit is Citadel local mode's co-browse audit log (aceteam#13503
// design section 2.6): append-only JSONL under <local state dir>/cobrowse-audit,
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
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// ensureDir creates or verifies the owner-only audit dir (a protected
// owner-only DACL on Windows).
var ensureDir = platform.EnsurePrivateDir

// DirName is the audit directory under the local-mode state dir (cmd.localStateDir).
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
	// URL is the target's scheme and host only; PathHash is the first 16 hex
	// characters of the SHA-256 of its path, so the same page can be
	// recognized without recording tokens a path or query may carry.
	URL      string `json:"url,omitempty"`
	PathHash string `json:"path_hash,omitempty"`
	Selector string `json:"selector,omitempty"`
	TextLen  int    `json:"text_len,omitempty"`
	// ResultingAction is the outcome: ok, refused, error, or a lifecycle note.
	ResultingAction string `json:"resulting_action"`
	// PendingSeq links a write's outcome to the pending record written
	// before the write ran.
	PendingSeq int64  `json:"pending_seq,omitempty"`
	Error      string `json:"error,omitempty"`
	PrevHash   string `json:"prev_hash"`
	Hash       string `json:"hash"`
}

// hashRecord is the SHA-256 of the record's RFC 8785 (JCS) canonical JSON with
// Hash empty, via the same canonicalizer as AEP receipts, so a server-side
// verifier in any language can recompute it.
func hashRecord(r Record) (string, error) {
	r.Hash = ""
	b, err := aep.CanonicalizeJCS(r)
	if err != nil {
		return "", fmt.Errorf("canonicalize audit record: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// RedactURL returns the URL's scheme and host, and a hash of its path. A URL
// with no host (data:, javascript:, about:, a malformed string) keeps only its
// scheme, so nothing an opaque URL carries is recorded.
func RedactURL(raw string) (origin, pathHash string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return "invalid", ""
	}
	scheme := strings.ToLower(u.Scheme)
	if u.Host == "" {
		if scheme == "about" && strings.EqualFold(u.Opaque, "blank") {
			return "about:blank", ""
		}
		return scheme + ":", ""
	}
	origin = scheme + "://" + strings.ToLower(u.Host)
	if p := u.EscapedPath(); p != "" && p != "/" {
		sum := sha256.Sum256([]byte(p))
		pathHash = hex.EncodeToString(sum[:8])
	}
	return origin, pathHash
}

var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*:(//)?[^\s"'<>]+`)

// SanitizeError keeps an error's text but replaces every URL in it with its
// redacted form, and bounds its length.
func SanitizeError(msg string) string {
	msg = urlInText.ReplaceAllStringFunc(msg, func(m string) string {
		if !strings.Contains(m, "/") && !strings.HasPrefix(strings.ToLower(m), "data:") && !strings.HasPrefix(strings.ToLower(m), "javascript:") {
			return m // "approval_required:" and similar labels
		}
		o, h := RedactURL(m)
		if h != "" {
			return o + "/#" + h
		}
		return o
	})
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return msg
}

// Log is an open audit log.
type Log struct {
	dir       string
	retention time.Duration
	now       func() time.Time

	mu     sync.Mutex
	seq    int64
	prev   string
	anchor Anchor
	failed error
}

// Anchor is kept beside the log: the newest record, and the last record that
// retention removed, so a missing head or tail is detectable.
type Anchor struct {
	HeadSeq         int64  `json:"head_seq"`
	HeadHash        string `json:"head_hash"`
	PrunedSeq       int64  `json:"pruned_through_seq,omitempty"`
	PrunedHash      string `json:"pruned_through_hash,omitempty"`
	PrunedThroughAt string `json:"pruned_through_month,omitempty"`
}

const anchorFile = "anchor.json"

func readAnchor(dir string) (Anchor, error) {
	var a Anchor
	data, err := os.ReadFile(filepath.Join(dir, anchorFile))
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return a, fmt.Errorf("read audit anchor: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return a, fmt.Errorf("audit anchor is unreadable: %w", err)
	}
	return a, nil
}

func writeAnchor(dir string, a Anchor) error {
	data, _ := json.Marshal(a)
	tmp := filepath.Join(dir, anchorFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, anchorFile))
}

// anchorHeld reports whether the log still contains the anchored head: the
// newest record is at or after it, and the record at the anchored seq has the
// anchored hash (or retention already removed it, as the anchor records).
func anchorHeld(files []string, a Anchor, newest int64) bool {
	if newest < a.HeadSeq {
		return false
	}
	if a.HeadSeq <= a.PrunedSeq {
		return true
	}
	found := false
	for _, f := range files {
		_ = eachRecord(f, func(_ int, r Record, _ []byte) error {
			if r.Seq == a.HeadSeq && r.Hash == a.HeadHash {
				found = true
			}
			return nil
		})
	}
	return found
}

// quarantineTornTail moves an incomplete final line (no trailing newline, as
// a short write or a crash leaves) out of the log into a side file, so the
// chain can resume. It returns the side file's path, or "".
func quarantineTornTail(path string, now time.Time) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || data[len(data)-1] == '\n' {
		return "", err
	}
	cut := strings.LastIndexByte(string(data), '\n') + 1
	side := fmt.Sprintf("%s.torn-%s", path, now.UTC().Format("20060102T150405Z"))
	if err := os.WriteFile(side, data[cut:], 0o600); err != nil {
		return "", fmt.Errorf("quarantine torn audit line: %w", err)
	}
	if err := os.Truncate(path, int64(cut)); err != nil {
		return "", fmt.Errorf("quarantine torn audit line: %w", err)
	}
	return side, nil
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
	if err := ensureDir(dir); err != nil {
		return nil, fmt.Errorf("audit dir %s: %w", dir, err)
	}
	l := &Log{dir: dir, retention: retention, now: now}
	// Resume the chain head BEFORE retention, so pruning every old month never
	// restarts the sequence or orphans the next record's link.
	files, err := monthFiles(dir)
	if err != nil {
		return nil, err
	}
	var repaired string
	if len(files) > 0 {
		if repaired, err = quarantineTornTail(files[len(files)-1], now()); err != nil {
			return nil, err
		}
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
	// The chain head must match the anchor (one record ahead is allowed: a
	// crash between the record and the anchor write).
	a, aerr := readAnchor(dir)
	if aerr != nil {
		return nil, aerr
	}
	switch {
	case l.seq == 0 && a.HeadSeq != 0 && a.PrunedSeq == a.HeadSeq && a.PrunedHash == a.HeadHash:
		// Retention removed every month file: continue the chain from the
		// anchor rather than restarting it.
		l.seq, l.prev = a.HeadSeq, a.HeadHash
	case a.HeadSeq != 0:
		if !anchorHeld(files, a, l.seq) {
			return nil, fmt.Errorf("audit log %s does not match its anchor (newest seq %d, anchored seq %d): records were removed or edited; keep a copy, then move the directory aside to start a new log", dir, l.seq, a.HeadSeq)
		}
	}
	l.anchor = a
	// Retention is housekeeping: a file a reader or antivirus holds open must
	// not stop co-browse.
	if err := l.prune(); err != nil {
		log.Printf("[local audit] retention skipped: %v", err)
	}
	if repaired != "" {
		if _, err := l.Append(Record{Actor: "citadel", IntendedAction: "repair", Class: "L",
			ResultingAction: "torn_tail_quarantined", Error: "an incomplete final line was moved to " + filepath.Base(repaired)}); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// Dir is the audit directory.
func (l *Log) Dir() string { return l.dir }

// Append fills in the chain fields and writes r durably.
// appendFile is the slice of *os.File Append uses; a seam so tests can
// simulate a disk filling up mid-write.
type appendFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

var openAppendFile = func(path string) (appendFile, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// ErrLogFailed is returned by Append after a write failed: the log refuses
// further records in this process, so a sequence number is never reused and
// nothing is chained onto a record that may not be on disk. Reopening (a new
// Citadel process) resumes from what is on disk.
var ErrLogFailed = errors.New("the audit log stopped after a failed write")

// Append fills in the chain fields and writes r durably. If the write or sync
// fails, the file is truncated back to its size before the write (so no
// partial record remains) and the log stops accepting records.
func (l *Log) Append(r Record) (Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failed != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrLogFailed, l.failed)
	}
	now := l.now().UTC()
	r.V = 1
	r.Source = "local"
	r.Seq = l.seq + 1
	r.CreatedAt = now.Format(time.RFC3339Nano)
	r.URL, r.PathHash = RedactURL(r.URL)
	r.Error = SanitizeError(r.Error)
	r.PrevHash = l.prev
	h, err := hashRecord(r)
	if err != nil {
		return Record{}, err
	}
	r.Hash = h
	line, err := json.Marshal(r)
	if err != nil {
		return Record{}, err
	}
	path := filepath.Join(l.dir, now.Format("2006-01")+".jsonl")
	var before int64
	if fi, err := os.Stat(path); err == nil {
		before = fi.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return Record{}, fmt.Errorf("stat audit log: %w", err)
	}
	f, err := openAppendFile(path)
	if err != nil {
		return Record{}, fmt.Errorf("open audit log: %w", err)
	}
	fail := func(what string, cause error) (Record, error) {
		_ = f.Close()
		if terr := os.Truncate(path, before); terr != nil {
			cause = fmt.Errorf("%v; and could not remove the partial record: %v", cause, terr)
		}
		l.failed = fmt.Errorf("%s: %w", what, cause)
		return Record{}, l.failed
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fail("write audit log", err)
	}
	if err := f.Sync(); err != nil {
		return fail("sync audit log", err)
	}
	if err := f.Close(); err != nil {
		return fail("close audit log", err)
	}
	l.seq, l.prev = r.Seq, r.Hash
	l.anchor.HeadSeq, l.anchor.HeadHash = r.Seq, r.Hash
	if err := writeAnchor(l.dir, l.anchor); err != nil {
		// The record is durable; a stale anchor only lags it, which Open and
		// Verify accept.
		return r, fmt.Errorf("write audit anchor: %w", err)
	}
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
			last, ok, err := lastRecord(f)
			if err != nil {
				return fmt.Errorf("apply audit retention to %s: %w", filepath.Base(f), err)
			}
			if ok {
				l.mu.Lock()
				prev := l.anchor
				l.anchor.PrunedSeq, l.anchor.PrunedHash = last.Seq, last.Hash
				l.anchor.PrunedThroughAt = month.Format("2006-01")
				if err := writeAnchor(l.dir, l.anchor); err != nil {
					l.anchor = prev
					l.mu.Unlock()
					return fmt.Errorf("apply audit retention: %w", err)
				}
				l.mu.Unlock()
			}
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
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
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
	// Truncated is true when the oldest retained record is not seq 1 and the
	// gap is exactly what retention recorded in the anchor.
	Truncated bool
}

// Verify recomputes every hash and link, and checks both ends of the chain
// against the anchor: the newest record must be the anchored head, and a
// missing start must be exactly the months retention removed. It returns the
// first problem found, naming the file and line.
func Verify(dir string) (VerifyResult, error) {
	var res VerifyResult
	files, err := monthFiles(dir)
	if err != nil {
		return res, err
	}
	a, err := readAnchor(dir)
	if err != nil {
		return res, err
	}
	prev := ""
	anchorSeen := false
	for _, f := range files {
		err := eachRecord(f, func(line int, r Record, _ []byte) error {
			where := fmt.Sprintf("%s:%d (seq %d)", filepath.Base(f), line, r.Seq)
			got, herr := hashRecord(r)
			if herr != nil {
				return fmt.Errorf("%s: %v", where, herr)
			}
			if got != r.Hash {
				return fmt.Errorf("%s: record was modified (hash mismatch)", where)
			}
			if r.Seq == a.HeadSeq && r.Hash == a.HeadHash {
				anchorSeen = true
			}
			if res.Records == 0 {
				res.FirstSeq = r.Seq
				switch {
				case r.Seq == 1:
					if r.PrevHash != "" {
						return fmt.Errorf("%s: first record links to a missing predecessor", where)
					}
				case a.PrunedSeq == r.Seq-1 && a.PrunedHash == r.PrevHash && line == 1:
					res.Truncated = true
				default:
					return fmt.Errorf("%s: records before this one are missing and were not removed by retention", where)
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
	switch {
	case a.HeadSeq == 0:
	case res.Records == 0:
		if !(a.PrunedSeq == a.HeadSeq && a.PrunedHash == a.HeadHash) {
			return res, fmt.Errorf("the log is empty but its anchor records %d entries: records were removed", a.HeadSeq)
		}
		res.Truncated = true
	case res.LastSeq < a.HeadSeq:
		return res, fmt.Errorf("the newest record (seq %d) is older than the anchored head (seq %d): records were removed from the end", res.LastSeq, a.HeadSeq)
	case a.HeadSeq > a.PrunedSeq && !anchorSeen:
		return res, fmt.Errorf("the anchored record (seq %d) is missing or was changed", a.HeadSeq)
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
