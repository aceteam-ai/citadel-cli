package footprint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }
func ip(v int) *int          { return &v }

func TestAppendWritesHeaderOnce(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	day := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	batch := []Sample{
		{Timestamp: day, NodeID: "node-a", Service: "vllm", Running: true, CPUPercent: f64(12.5), RSSMB: f64(7580)},
		{Timestamp: day, NodeID: "node-a", Service: NodeService, Running: true, VRAMMB: ip(7400), GPUUtilPercent: f64(0)},
	}
	if err := store.Append(batch); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Append a second batch to the same day: header must NOT be repeated.
	if err := store.Append([]Sample{{Timestamp: day.Add(time.Minute), NodeID: "node-a", Service: "vllm", Running: true, RSSMB: f64(7600)}}); err != nil {
		t.Fatalf("Append 2: %v", err)
	}

	path := filepath.Join(dir, "footprints-2026-07-01.csv")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 { // header + 3 data rows
		t.Fatalf("expected 4 lines (1 header + 3 rows), got %d: %q", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "ts,node_id,service,running,cpu_pct,rss_mb,vram_mb,gpu_util_pct,idle_seconds") {
		t.Fatalf("unexpected header: %q", lines[0])
	}
	if strings.Contains(strings.Join(lines[1:], "\n"), "ts,node_id") {
		t.Fatalf("header repeated in data rows")
	}
	// Unset optional metrics must render as blank, not "0".
	if !strings.Contains(lines[1], "12.50,7580.00,,,") {
		t.Fatalf("expected blank vram/gpu/idle for vllm row, got %q", lines[1])
	}
}

func TestAppendRotatesByDay(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)

	d1 := time.Date(2026, 7, 1, 23, 59, 0, 0, time.UTC)
	d2 := time.Date(2026, 7, 2, 0, 1, 0, 0, time.UTC)
	if err := store.Append([]Sample{{Timestamp: d1, NodeID: "n", Service: "svc"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append([]Sample{{Timestamp: d2, NodeID: "n", Service: "svc"}}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"footprints-2026-07-01.csv", "footprints-2026-07-02.csv"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected daily file %s: %v", name, err)
		}
	}
}

func TestAppendEmptyIsNoop(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	if err := store.Append(nil); err != nil {
		t.Fatalf("Append(nil): %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("expected no files, got %d", len(entries))
	}
}

func TestPruneRemovesOldKeepsRecent(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)

	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	// Create files spanning old and recent days plus an unrelated file.
	days := []time.Time{
		now.AddDate(0, 0, -10), // old -> pruned (retention 7)
		now.AddDate(0, 0, -7),  // exactly at boundary -> pruned (keeps 7 incl today)
		now.AddDate(0, 0, -6),  // kept
		now,                    // kept
	}
	for _, d := range days {
		if err := store.Append([]Sample{{Timestamp: d, NodeID: "n", Service: "svc"}}); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(unrelated, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	pruned, err := store.Prune(now, 7)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(pruned) != 2 {
		t.Fatalf("expected 2 pruned files, got %d: %v", len(pruned), pruned)
	}

	// Recent files + the unrelated file survive.
	for _, keep := range []string{
		dailyFileName(now.AddDate(0, 0, -6)),
		dailyFileName(now),
		"notes.txt",
	} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Fatalf("expected %s to survive prune: %v", keep, err)
		}
	}
	// Old files are gone.
	for _, gone := range []string{
		dailyFileName(now.AddDate(0, 0, -10)),
		dailyFileName(now.AddDate(0, 0, -7)),
	} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be pruned", gone)
		}
	}
}

func TestPruneDisabledWhenRetentionNonPositive(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	now := time.Now()
	if err := store.Append([]Sample{{Timestamp: now.AddDate(0, 0, -100), NodeID: "n", Service: "svc"}}); err != nil {
		t.Fatal(err)
	}
	pruned, err := store.Prune(now, 0)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(pruned) != 0 {
		t.Fatalf("retention<=0 should prune nothing, got %v", pruned)
	}
}
