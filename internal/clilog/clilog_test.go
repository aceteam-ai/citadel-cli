package clilog

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// reset returns the package to a clean state and points it at a temp dir with
// a fixed clock. It returns the temp dir.
func reset(t *testing.T, now time.Time) string {
	t.Helper()
	mu.Lock()
	if file != nil {
		_ = file.Close()
	}
	file = nil
	fileDate = ""
	headerOnce = sync.Once{}
	dir := t.TempDir()
	dirOverride = dir
	nowFn = func() time.Time { return now }
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		if file != nil {
			_ = file.Close()
			file = nil
		}
		dirOverride = ""
		nowFn = time.Now
		fileDate = ""
		mu.Unlock()
	})
	return dir
}

func TestPathIsDateBased(t *testing.T) {
	day := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	reset(t, day)
	got := filepath.Base(Path())
	if got != "citadel-2026-06-29.log" {
		t.Fatalf("Path basename = %q, want citadel-2026-06-29.log", got)
	}
}

func TestSameDayAppendsToOneFile(t *testing.T) {
	day := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	dir := reset(t, day)

	Write("", "first")
	Write("warning", "second")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var logs []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "citadel-") {
			logs = append(logs, e.Name())
		}
	}
	if len(logs) != 1 {
		t.Fatalf("expected exactly one dated log file, got %v", logs)
	}

	data, err := os.ReadFile(filepath.Join(dir, "citadel-2026-06-29.log"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "=== Session started ===") {
		t.Errorf("missing session header in:\n%s", s)
	}
	if !strings.Contains(s, "[CITADEL] first") {
		t.Errorf("missing plain entry in:\n%s", s)
	}
	if !strings.Contains(s, "[warning] second") {
		t.Errorf("missing leveled entry in:\n%s", s)
	}
	if strings.Count(s, "Session started") != 1 {
		t.Errorf("expected exactly one session header, got:\n%s", s)
	}
}

func TestRotatesAcrossMidnight(t *testing.T) {
	day1 := time.Date(2026, 6, 29, 23, 59, 0, 0, time.UTC)
	dir := reset(t, day1)

	Write("", "before midnight")

	// Advance the clock to the next day and write again.
	mu.Lock()
	nowFn = func() time.Time { return time.Date(2026, 6, 30, 0, 1, 0, 0, time.UTC) }
	mu.Unlock()
	Write("", "after midnight")

	if _, err := os.Stat(filepath.Join(dir, "citadel-2026-06-29.log")); err != nil {
		t.Errorf("day-1 file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "citadel-2026-06-30.log")); err != nil {
		t.Errorf("day-2 file missing (no rotation): %v", err)
	}
}

func TestLatestSymlinkPointsAtToday(t *testing.T) {
	day := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	dir := reset(t, day)
	Write("", "hi")

	target, err := os.Readlink(filepath.Join(dir, "latest.log"))
	if err != nil {
		t.Fatalf("latest.log not a symlink: %v", err)
	}
	if target != "citadel-2026-06-29.log" {
		t.Errorf("latest.log -> %q, want citadel-2026-06-29.log", target)
	}
}
