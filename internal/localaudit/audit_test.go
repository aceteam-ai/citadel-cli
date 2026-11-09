package localaudit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func writeN(t *testing.T, l *Log, n int, session string) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := l.Append(Record{SessionID: session, Actor: "agent", IntendedAction: "click", Class: "W", ResultingAction: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChainVerifiesAndResumesAcrossOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	now := time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)
	l, err := Open(dir, 0, fixedClock(now))
	if err != nil {
		t.Fatal(err)
	}
	writeN(t, l, 3, "cb-1")
	// A month boundary and a reopen must continue the same chain.
	l2, err := Open(dir, 0, fixedClock(now.Add(2*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	writeN(t, l2, 2, "cb-2")
	res, err := Verify(dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Records != 5 || res.FirstSeq != 1 || res.LastSeq != 5 || res.Truncated {
		t.Fatalf("verify result: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-12.jsonl")); err != nil {
		t.Fatal("records should roll into the new month's file")
	}
	recs, _ := Read(dir, Filter{SessionID: "cb-2"})
	if len(recs) != 2 || recs[0].Source != "local" {
		t.Fatalf("read filter: %+v", recs)
	}
	if recs, _ := Read(dir, Filter{Limit: 2}); len(recs) != 2 || recs[1].Seq != 5 {
		t.Fatalf("read limit: %+v", recs)
	}
}

func TestVerifyDetectsEditRemovalAndReorder(t *testing.T) {
	mk := func(t *testing.T) (string, string) {
		dir := filepath.Join(t.TempDir(), "audit")
		l, err := Open(dir, 0, fixedClock(time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)))
		if err != nil {
			t.Fatal(err)
		}
		writeN(t, l, 4, "cb-1")
		return dir, filepath.Join(dir, "2026-11.jsonl")
	}
	lines := func(p string) []string {
		b, _ := os.ReadFile(p)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	put := func(p string, ls []string) { _ = os.WriteFile(p, []byte(strings.Join(ls, "\n")+"\n"), 0o600) }

	t.Run("edited", func(t *testing.T) {
		dir, p := mk(t)
		ls := lines(p)
		ls[1] = strings.Replace(ls[1], `"resulting_action":"ok"`, `"resulting_action":"refused"`, 1)
		put(p, ls)
		if _, err := Verify(dir); err == nil || !strings.Contains(err.Error(), "modified") {
			t.Fatalf("want modified, got %v", err)
		}
	})
	t.Run("removed", func(t *testing.T) {
		dir, p := mk(t)
		ls := lines(p)
		put(p, append(ls[:1], ls[2:]...))
		if _, err := Verify(dir); err == nil || !strings.Contains(err.Error(), "chain broken") {
			t.Fatalf("want chain broken, got %v", err)
		}
	})
	t.Run("reordered", func(t *testing.T) {
		dir, p := mk(t)
		ls := lines(p)
		ls[1], ls[2] = ls[2], ls[1]
		put(p, ls)
		if _, err := Verify(dir); err == nil {
			t.Fatal("reorder must be detected")
		}
	})

}

func TestRetentionRemovesOldMonths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	old, err := Open(dir, 0, fixedClock(time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	writeN(t, old, 1, "cb-old")
	recent, err := Open(dir, 0, fixedClock(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	writeN(t, recent, 1, "cb-new")
	if _, err := Open(dir, 0, fixedClock(time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-06.jsonl")); !os.IsNotExist(err) {
		t.Fatal("a month older than 90 days should be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-10.jsonl")); err != nil {
		t.Fatal("a recent month must be kept")
	}
	res, err := Verify(dir)
	if err != nil || !res.Truncated || res.FirstSeq != 2 {
		t.Fatalf("verify after retention: %+v %v", res, err)
	}
	a, _ := readAnchor(dir)
	if a.PrunedSeq != 1 {
		t.Fatalf("retention must record what it removed: %+v", a)
	}
}

func TestRedactURLAndErrors(t *testing.T) {
	for in, want := range map[string][2]string{
		"https://user:pw@shop.test/reset/TOKEN123?token=abc#frag": {"https://shop.test", "nonempty"},
		"http://a.test/":             {"http://a.test", ""},
		"about:blank":                {"about:blank", ""},
		"data:text/plain,SECRET":     {"data:", ""},
		"javascript:alert('SECRET')": {"javascript:", ""},
		"":                           {"", ""},
	} {
		o, h := RedactURL(in)
		if o != want[0] || (want[1] == "" && h != "") || (want[1] == "nonempty" && len(h) != 16) {
			t.Errorf("%q: got %q %q", in, o, h)
		}
	}
	msg := SanitizeError(`navigation refused: http://shop.test/reset/TOKEN123?x=SECRET and data:text/plain,SECRET`)
	if strings.Contains(msg, "TOKEN123") || strings.Contains(msg, "SECRET") || !strings.Contains(msg, "navigation refused") {
		t.Fatalf("error not sanitized: %s", msg)
	}
}

func TestTornTailIsQuarantinedOnOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	clock := fixedClock(time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC))
	l, err := Open(dir, 0, clock)
	if err != nil {
		t.Fatal(err)
	}
	writeN(t, l, 2, "cb-1")
	p := filepath.Join(dir, "2026-11.jsonl")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"v":1,"seq":3,"created_at":"2026-11-0`)
	f.Close()
	l2, err := Open(dir, 0, clock)
	if err != nil {
		t.Fatalf("a torn final line must not wedge the log: %v", err)
	}
	writeN(t, l2, 1, "cb-1")
	res, err := Verify(dir)
	if err != nil || res.Records != 4 {
		t.Fatalf("verify after repair: %+v %v", res, err)
	}
	if side, _ := filepath.Glob(p + ".torn-*"); len(side) != 1 {
		t.Fatal("the torn line should be kept in a side file")
	}
	recs, _ := Read(dir, Filter{})
	if recs[2].IntendedAction != "repair" {
		t.Fatalf("the repair should be recorded: %+v", recs[2])
	}
}

func TestVerifyDetectsMissingTailAndUnretainedHead(t *testing.T) {
	mk := func(t *testing.T) (string, string) {
		dir := filepath.Join(t.TempDir(), "audit")
		l, err := Open(dir, 0, fixedClock(time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)))
		if err != nil {
			t.Fatal(err)
		}
		writeN(t, l, 4, "cb-1")
		return dir, filepath.Join(dir, "2026-11.jsonl")
	}
	lines := func(p string) []string {
		b, _ := os.ReadFile(p)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	put := func(p string, ls []string) { _ = os.WriteFile(p, []byte(strings.Join(ls, "\n")+"\n"), 0o600) }

	dir, p := mk(t)
	put(p, lines(p)[:2])
	if _, err := Verify(dir); err == nil || !strings.Contains(err.Error(), "removed from the end") {
		t.Fatalf("a removed tail must be detected: %v", err)
	}
	if _, err := Open(dir, 0, nil); err == nil {
		t.Fatal("Open must not chain onto a shortened tail")
	}

	dir, p = mk(t)
	put(p, lines(p)[1:])
	if _, err := Verify(dir); err == nil || !strings.Contains(err.Error(), "not removed by retention") {
		t.Fatalf("a removed head that retention did not remove must be detected: %v", err)
	}

	dir, p = mk(t)
	ls := lines(p)
	ls[1] = strings.Replace(ls[1], `"v":1,`, `"v":1,"extra":"x",`, 1)
	put(p, ls)
	if _, err := Verify(dir); err == nil {
		t.Fatal("an unknown field must be rejected")
	}
}

// shortWriter writes half of each buffer and then reports a full disk.
type shortWriter struct{ f *os.File }

func (w shortWriter) Write(b []byte) (int, error) {
	n, _ := w.f.Write(b[:len(b)/2])
	return n, syscall.ENOSPC
}
func (w shortWriter) Sync() error  { return w.f.Sync() }
func (w shortWriter) Close() error { return w.f.Close() }

func TestDiskFullMidWriteRollsBackAndReopens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	clock := fixedClock(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	l, err := Open(dir, 0, clock)
	if err != nil {
		t.Fatal(err)
	}
	writeN(t, l, 2, "cb-1")
	prev := openAppendFile
	openAppendFile = func(path string) (appendFile, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		return shortWriter{f}, err
	}
	_, err = l.Append(Record{SessionID: "cb-1", Actor: "agent", IntendedAction: "click", Class: "W", ResultingAction: "pending"})
	openAppendFile = prev
	if err == nil {
		t.Fatal("a short write must fail")
	}
	// The next record (for example the user's Stop) is refused in this
	// process: no sequence number is reused.
	if _, err := l.Append(Record{Actor: "user", IntendedAction: "stop", Class: "H", ResultingAction: "ok"}); !errors.Is(err, ErrLogFailed) {
		t.Fatalf("append after a failed write: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "2026-11.jsonl")); !strings.HasSuffix(string(b), "\n") {
		t.Fatal("the partial record must be removed")
	}
	l2, err := Open(dir, 0, clock)
	if err != nil {
		t.Fatalf("reopen after a failed write: %v", err)
	}
	r, err := l2.Append(Record{Actor: "user", IntendedAction: "stop", Class: "H", ResultingAction: "ok"})
	if err != nil || r.Seq != 3 {
		t.Fatalf("append after reopen: %+v %v", r, err)
	}
	if res, err := Verify(dir); err != nil || res.Records != 3 {
		t.Fatalf("verify: %+v %v", res, err)
	}
}

func TestAnchorLagAndFullRetentionResume(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	l, err := Open(dir, 0, fixedClock(time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	writeN(t, l, 3, "cb-1")
	// A stale anchor (two records behind, e.g. failed anchor writes) is fine
	// while the anchored record is still there with its hash.
	a, _ := readAnchor(dir)
	recs, _ := Read(dir, Filter{})
	a.HeadSeq, a.HeadHash = recs[0].Seq, recs[0].Hash
	_ = writeAnchor(dir, a)
	if _, err := Verify(dir); err != nil {
		t.Fatalf("a lagging anchor must verify: %v", err)
	}
	if _, err := Open(dir, 0, fixedClock(time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC))); err != nil {
		t.Fatalf("a lagging anchor must open: %v", err)
	}
	// Retention removes every month: the chain resumes from the anchor.
	l2, err := Open(dir, 0, fixedClock(time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	writeN(t, l2, 1, "cb-1") // anchor head back at seq 4
	l3, err := Open(dir, 0, fixedClock(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("open after every month was removed: %v", err)
	}
	if files, _ := monthFiles(dir); len(files) != 0 {
		t.Fatalf("retention should have removed every month: %v", files)
	}
	r, err := l3.Append(Record{Actor: "agent", IntendedAction: "start", Class: "L", ResultingAction: "ok"})
	if err != nil || r.Seq != 5 {
		t.Fatalf("the chain must continue from the anchor: %+v %v", r, err)
	}
	if res, err := Verify(dir); err != nil || !res.Truncated || res.FirstSeq != 5 {
		t.Fatalf("verify after full retention: %+v %v", res, err)
	}
}
