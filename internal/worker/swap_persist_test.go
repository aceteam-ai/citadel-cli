package worker

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// fakeClock lets a test pin exact, orderable timestamps instead of relying on
// wall-clock sleeps between touches.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// newPersistTestManager builds a manager with persistence enabled at path,
// fast test timing, and persistMinGap=0 so every touch/markReady flushes
// synchronously (deterministic — no reliance on real-time debounce windows).
func newPersistTestManager(ctrl SwapController, path string) *SwapManager {
	m := newTestManager(ctrl)
	WithPersistence(path, nil)(m)
	m.persistMinGap = 0
	return m
}

// TestLastUsed_SurvivesSimulatedRestart is the primary #688 contract: persist,
// then construct a FRESH SwapManager (simulating a worker restart) pointed at
// the same file, and confirm sortByLRU orders candidates by the recency that
// was loaded from disk rather than treating everything as equally unused.
func TestLastUsed_SurvivesSimulatedRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swap-lru.json")
	clock := newFakeClock(time.Now())

	ctrl := newMockController()
	m1 := newPersistTestManager(ctrl, path)
	m1.now = clock.Now

	// bonsai used first (oldest), then vllm, then unlimited-ocr (most recent).
	m1.touch("bonsai")
	clock.Advance(time.Minute)
	m1.touch("vllm")
	clock.Advance(time.Minute)
	m1.touch("unlimited-ocr")

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected persisted file to exist after touches, stat error: %v", err)
	}

	// Simulate a restart: a brand-new manager, no in-process state, loading the
	// same file.
	ctrl2 := newMockController()
	m2 := newPersistTestManager(ctrl2, path)
	m2.now = clock.Now

	candidates := []status.PreemptCandidate{
		{Name: "unlimited-ocr", VRAMBytes: 1},
		{Name: "bonsai", VRAMBytes: 1},
		{Name: "vllm", VRAMBytes: 1},
	}
	m2.sortByLRU(candidates)

	got := []string{candidates[0].Name, candidates[1].Name, candidates[2].Name}
	want := []string{"bonsai", "vllm", "unlimited-ocr"} // least-recently-used first
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LRU order after simulated restart = %v, want %v (recency did not survive persist/reload)", got, want)
		}
	}
}

// TestLastUsed_NoPersistedFile_LoadsEmpty confirms a fresh node (no file yet)
// starts with no recency rather than erroring — the seed case every real first
// boot goes through.
func TestLastUsed_NoPersistedFile_LoadsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	loaded := loadLastUsedFile(path)
	if len(loaded) != 0 {
		t.Fatalf("expected empty map for a missing file, got %v", loaded)
	}
}

// TestLastUsed_CorruptFile_LoadsEmptyNotError pins the lenient-parse contract:
// a truncated/corrupt persisted file must never block startup or a swap
// decision. Mirrors the #815 TokenHashEntry reasoning cited in swap_persist.go.
func TestLastUsed_CorruptFile_LoadsEmptyNotError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swap-lru.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	loaded := loadLastUsedFile(path)
	if len(loaded) != 0 {
		t.Fatalf("expected corrupt file to degrade to empty map, got %v", loaded)
	}
}

// TestForget_PreservesLastUsed pins the OTHER half of #688's "fixed forget":
// eviction must NOT drop lastUsed. Verified against git history that forget()
// never did this, but a passing test here is what stops a future edit from
// quietly reintroducing exactly the thrash #688 describes (evict A, forget A,
// A now looks like it was never used, A is evicted again before it can serve).
func TestForget_PreservesLastUsed(t *testing.T) {
	ctrl := newMockController()
	m := newTestManager(ctrl)

	m.touch("bonsai")
	m.mu.Lock()
	before, ok := m.lastUsed["bonsai"]
	m.mu.Unlock()
	if !ok {
		t.Fatal("expected lastUsed entry after touch")
	}

	m.forget("bonsai")

	m.mu.Lock()
	after, stillThere := m.lastUsed["bonsai"]
	m.mu.Unlock()
	if !stillThere {
		t.Fatal("forget() must NOT delete the lastUsed entry — an evicted engine must re-enter as recently used, not never used (citadel-cli#688)")
	}
	if !after.Equal(before) {
		t.Fatalf("forget() must not modify the lastUsed timestamp either, got %v want %v", after, before)
	}
}

// TestPruneStaleLastUsed_RemovesOldEntries reproduces the "forget" gap #688
// asks for: once lastUsed is durable, an entry for a long-gone engine (an
// uninstalled/renamed backend nothing touches again) must not survive forever.
// A stale entry sitting in the map is otherwise harmless to eviction ordering
// (PreemptInputs never lists a gone engine as a candidate), but nothing bounded
// its growth before this — this test reproduces that unbounded state, then
// asserts pruneStaleLastUsedLocked (wired into every flushPersist) removes it.
func TestPruneStaleLastUsed_RemovesOldEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swap-lru.json")
	clock := newFakeClock(time.Now())

	ctrl := newMockController()
	m := newPersistTestManager(ctrl, path)
	m.now = clock.Now

	// Reproduce the stale entry: an engine touched once, long ago, never again
	// (the "uninstalled and gone" case).
	m.mu.Lock()
	m.lastUsed["ghost-engine"] = clock.Now().Add(-lastUsedRetention - time.Hour)
	m.lastUsed["fresh-engine"] = clock.Now()
	m.mu.Unlock()

	// Before the fix's prune runs, both entries are present (reproducing the
	// gap: nothing bounds growth).
	m.mu.Lock()
	_, ghostPresentBeforePrune := m.lastUsed["ghost-engine"]
	m.mu.Unlock()
	if !ghostPresentBeforePrune {
		t.Fatal("setup invariant broken: stale entry should be present before any prune runs")
	}

	// Any flush (touch/markReady both call persistIfDue -> flushPersist) prunes
	// stale entries as part of its snapshot phase.
	m.touch("fresh-engine")

	m.mu.Lock()
	_, ghostStillPresent := m.lastUsed["ghost-engine"]
	_, freshStillPresent := m.lastUsed["fresh-engine"]
	m.mu.Unlock()
	if ghostStillPresent {
		t.Fatal("expected the stale (>retention) lastUsed entry to be pruned, but it survived a flush")
	}
	if !freshStillPresent {
		t.Fatal("prune must not remove a fresh entry")
	}

	// And the persisted file itself must not carry the stale entry either.
	onDisk := loadLastUsedFile(path)
	if _, present := onDisk["ghost-engine"]; present {
		t.Fatal("expected the stale entry to be absent from the persisted file")
	}
	if _, present := onDisk["fresh-engine"]; !present {
		t.Fatal("expected the fresh entry to be present in the persisted file")
	}
}

// TestSwap_ConcurrentTouchAndPersist_NoRace exercises touch (writes lastUsed +
// triggers a debounced persist) and an explicit flushPersist concurrently, to
// be run under -race. It does not assert on the final file contents (a lost
// update under real concurrency is an accepted, documented best-effort
// tradeoff — see swap_persist.go) — only that nothing races and nothing panics.
func TestSwap_ConcurrentTouchAndPersist_NoRace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swap-lru.json")

	ctrl := newMockController()
	m := newPersistTestManager(ctrl, path)

	backends := []string{"bonsai", "vllm", "unlimited-ocr", "ollama"}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		backend := backends[i%len(backends)]
		go func(b string) {
			defer wg.Done()
			m.touch(b)
		}(backend)
		go func() {
			defer wg.Done()
			m.flushPersist()
		}()
	}
	wg.Wait()

	// One final flush so the file reflects SOME consistent state, then confirm
	// it loads back cleanly (no torn/corrupt write from the concurrent writers).
	m.flushPersist()
	loaded := loadLastUsedFile(path)
	if len(loaded) == 0 {
		t.Fatal("expected at least one lastUsed entry to have survived concurrent touch+persist")
	}
}

// TestPersist_UnwritableDir_IsNonFatal is the "best-effort" contract: a persist
// failure (here, a path whose parent cannot be created because a FILE occupies
// that path component) must never surface as a swap error or panic — it is
// logged and otherwise ignored.
func TestPersist_UnwritableDir_IsNonFatal(t *testing.T) {
	dir := t.TempDir()
	// Create a FILE where the persist path wants a DIRECTORY, so MkdirAll fails.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	badPath := filepath.Join(blocker, "nested", "swap-lru.json")

	var loggedErr string
	logf := func(format string, args ...any) {
		loggedErr = format
		_ = args
	}

	ctrl := newMockController()
	m := newTestManager(ctrl)
	WithPersistence(badPath, logf)(m)
	m.persistMinGap = 0

	// Must not panic, and EnsureResident must still work normally despite the
	// unwritable persist path.
	out, err := m.EnsureResident(context.Background(), "bonsai", "Bonsai-27B")
	if err != nil {
		t.Fatalf("unexpected hard error from EnsureResident despite persist failure: %v", err)
	}
	_ = out

	if loggedErr == "" {
		t.Fatal("expected the persist failure to be logged (best-effort, not silent)")
	}

	// The write itself must report an error too (direct check, not just via the
	// log callback).
	if err := writeLastUsedFile(badPath, map[string]time.Time{"bonsai": time.Now()}); err == nil {
		t.Fatal("expected writeLastUsedFile to fail against an unwritable parent path")
	}
}
