package localaudit

import (
	"os"
	"path/filepath"
	"strings"
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
	dir := t.TempDir()
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
		dir := t.TempDir()
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
	t.Run("truncated head", func(t *testing.T) {
		dir, p := mk(t)
		put(p, lines(p)[1:])
		res, err := Verify(dir)
		if err != nil || !res.Truncated {
			t.Fatalf("a removed head reads as retention truncation: %+v %v", res, err)
		}
	})
}

func TestRetentionRemovesOldMonths(t *testing.T) {
	dir := t.TempDir()
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
}

func TestSanitizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pw@shop.test/orders?token=abc#frag": "https://shop.test/orders",
		"http://a.test/": "http://a.test/",
		"about:blank":    "about:blank",
		"":               "",
	} {
		if got := SanitizeURL(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}
