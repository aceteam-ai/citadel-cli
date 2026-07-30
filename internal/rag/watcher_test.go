package rag

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWatcherIgnoresIndexDBEvents locks the self-trigger guard: the watcher must
// treat the index DB (and its WAL/SHM/journal siblings) as ignorable, so a root
// that contains the index cannot create an index -> DB-write -> event -> index
// loop.
func TestWatcherIgnoresIndexDBEvents(t *testing.T) {
	t.Setenv("CITADEL_INDEX_DB", filepath.Join(t.TempDir(), "index.db"))
	w, err := NewWatcher([]string{t.TempDir()}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	db := w.svc.DBPath()
	for _, p := range []string{db, db + "-wal", db + "-shm", db + "-journal"} {
		if _, ok := w.dbPaths[p]; !ok {
			t.Errorf("watcher should ignore events on %s", p)
		}
	}
	// A normal file under a root must NOT be ignored.
	if _, ok := w.dbPaths[filepath.Join(t.TempDir(), "notes.md")]; ok {
		t.Error("a regular file must not be in the ignore set")
	}
}

// TestDebouncerCoalescesBurst asserts a burst of trigger() calls results in a
// single fn() invocation, fired after the last trigger.
func TestDebouncerCoalescesBurst(t *testing.T) {
	var calls int32
	d := newDebouncer(40*time.Millisecond, func() { atomic.AddInt32(&calls, 1) })
	defer d.stop()

	// 10 rapid triggers within one debounce window.
	for i := 0; i < 10; i++ {
		d.trigger()
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("fn should not have fired during the burst, got %d calls", got)
	}
	time.Sleep(120 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 coalesced call, got %d", got)
	}
}

// TestDebouncerSeparateBurstsFireSeparately asserts two bursts separated by more
// than the debounce delay each fire once.
func TestDebouncerSeparateBurstsFireSeparately(t *testing.T) {
	var calls int32
	d := newDebouncer(30*time.Millisecond, func() { atomic.AddInt32(&calls, 1) })
	defer d.stop()

	d.trigger()
	time.Sleep(80 * time.Millisecond) // let the first fire
	d.trigger()
	time.Sleep(80 * time.Millisecond) // let the second fire

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 calls for 2 separated bursts, got %d", got)
	}
}

// TestDebouncerStopCancelsPending asserts stop() prevents a pending invocation.
func TestDebouncerStopCancelsPending(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	d := newDebouncer(40*time.Millisecond, func() {
		mu.Lock()
		calls++
		mu.Unlock()
	})
	d.trigger()
	d.stop()
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 0 {
		t.Fatalf("stop() should cancel the pending call, got %d", got)
	}
	// trigger() after stop must be a no-op.
	d.trigger()
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	got = calls
	mu.Unlock()
	if got != 0 {
		t.Fatalf("trigger() after stop must be a no-op, got %d", got)
	}
}
