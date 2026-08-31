package cacheindex

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/services"
)

func TestRoundTripUpsertLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)

	s := Open(path, nil)
	e := Entry{
		CacheDir:  services.HFHubCacheDirName,
		Family:    services.CacheFamilyHFHub,
		Model:     "meta-llama/Llama-3.1-8B-Instruct",
		Engine:    "vllm",
		Files:     []string{"models--meta-llama--Llama-3.1-8B-Instruct"},
		SizeBytes: 16_800_000_000,
	}
	if err := s.Upsert(e); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected index file to be written: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := loaded.Lookup(services.HFHubCacheDirName, e.Model)
	if !ok {
		t.Fatalf("expected entry to round-trip")
	}
	if got.Engine != "vllm" || got.SizeBytes != e.SizeBytes || len(got.Files) != 1 {
		t.Errorf("round-tripped entry mismatch: %+v", got)
	}
	if got.PulledAt.IsZero() || got.LastUsed.IsZero() {
		t.Errorf("expected PulledAt/LastUsed to be defaulted to now on Upsert, got %+v", got)
	}
	if got.Source != SourcePull {
		t.Errorf("expected default Source %q, got %q", SourcePull, got.Source)
	}
}

func TestLoadMissingFileDegradesToEmpty(t *testing.T) {
	idx, err := Load(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatalf("expected no error for a missing file, got %v", err)
	}
	if len(idx.All()) != 0 {
		t.Fatalf("expected an empty index, got %d entries", len(idx.All()))
	}
}

func TestLoadCorruptFileDegradesToEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := Load(path)
	if err == nil {
		t.Fatalf("expected a parse error to be reported")
	}
	if len(idx.All()) != 0 {
		t.Fatalf("expected an empty index despite the corrupt file, got %d entries", len(idx.All()))
	}
}

func TestLoadSkipsOnlyTheMalformedEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	// One well-formed entry, one entry that is not even a JSON object, one
	// entry missing required key fields (cache_dir/model) -- only the good
	// entry should survive.
	raw := `{
		"version": 1,
		"entries": [
			{"cache_dir": "huggingface", "model": "org/repo", "engine": "vllm", "size_bytes": 42},
			"this is not an object",
			{"cache_dir": "", "model": "", "engine": "vllm"}
		]
	}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := Load(path)
	if err != nil {
		t.Fatalf("expected the malformed array element to be skipped, not fail the whole load: %v", err)
	}
	all := idx.All()
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 surviving entry, got %d: %+v", len(all), all)
	}
	if all[0].Model != "org/repo" {
		t.Errorf("unexpected surviving entry: %+v", all[0])
	}
}

func TestLoadLenientTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	raw := `{"version":1,"entries":[
		{"cache_dir":"huggingface","model":"org/repo","engine":"vllm","size_bytes":1,"pulled_at":"not-a-timestamp","last_used":"2026-08-20T10:00:00Z"}
	]}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, ok := idx.Lookup("huggingface", "org/repo")
	if !ok {
		t.Fatalf("expected the entry to survive a malformed pulled_at field")
	}
	if !e.PulledAt.IsZero() {
		t.Errorf("expected PulledAt to degrade to zero on a bad timestamp, got %v", e.PulledAt)
	}
	if e.LastUsed.IsZero() || e.LastUsed.Year() != 2026 {
		t.Errorf("expected LastUsed to parse correctly, got %v", e.LastUsed)
	}
}

func TestRemoveAndRemoveFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	s := Open(path, nil)

	if err := s.Upsert(Entry{
		CacheDir: services.LlamaCppCacheDirName,
		Family:   services.CacheFamilyGGUFDir,
		Model:    "TheBloke/Llama-2-7B-GGUF",
		Engine:   "llamacpp",
		Files:    []string{"llama-2-7b.Q4_K_M.gguf", "config.json"},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := s.RemoveFile(services.LlamaCppCacheDirName, "TheBloke/Llama-2-7B-GGUF", "config.json"); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	e, ok := s.Snapshot().Lookup(services.LlamaCppCacheDirName, "TheBloke/Llama-2-7B-GGUF")
	if !ok || len(e.Files) != 1 || e.Files[0] != "llama-2-7b.Q4_K_M.gguf" {
		t.Fatalf("expected only config.json removed, got %+v (ok=%v)", e, ok)
	}

	if err := s.RemoveFile(services.LlamaCppCacheDirName, "TheBloke/Llama-2-7B-GGUF", "llama-2-7b.Q4_K_M.gguf"); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if _, ok := s.Snapshot().Lookup(services.LlamaCppCacheDirName, "TheBloke/Llama-2-7B-GGUF"); ok {
		t.Fatalf("expected the entry to be dropped once its last file was removed")
	}

	if err := s.Upsert(Entry{CacheDir: "ollama", Family: services.CacheFamilyNative, Model: "llama3.1:8b", Engine: "ollama"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.Remove("ollama", "llama3.1:8b"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := s.Snapshot().Lookup("ollama", "llama3.1:8b"); ok {
		t.Fatalf("expected the ollama entry to be removed")
	}
	// Removing something absent must be a harmless no-op, not an error.
	if err := s.Remove("ollama", "llama3.1:8b"); err != nil {
		t.Fatalf("Remove on an absent entry should be a no-op, got %v", err)
	}
}

func TestMarkUsedOnlyTouchesExistingEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	s := Open(path, nil)

	// MarkUsed for a model with no entry must not fabricate one.
	s.MarkUsed("huggingface", "no/such-model", time.Now())
	if _, ok := s.Snapshot().Lookup("huggingface", "no/such-model"); ok {
		t.Fatalf("MarkUsed must never create a new entry")
	}

	if err := s.Upsert(Entry{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/repo", Engine: "vllm"}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Snapshot().Lookup("huggingface", "org/repo")

	later := before.LastUsed.Add(time.Hour)
	s.MarkUsed("huggingface", "org/repo", later)
	// MarkUsed's flush is debounced; the in-memory value should still update
	// immediately even if the disk write is deferred.
	after, ok := s.Snapshot().Lookup("huggingface", "org/repo")
	if !ok {
		t.Fatalf("entry disappeared after MarkUsed")
	}
	if !after.LastUsed.Equal(later) {
		t.Errorf("expected LastUsed to advance to %v, got %v", later, after.LastUsed)
	}
}

func TestVerifyDetectsStaleness(t *testing.T) {
	root := t.TempDir()
	hubDir := filepath.Join(root, services.HFHubCacheDirName, "hub", "models--org--repo")
	if err := os.MkdirAll(hubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hubDir, "weights.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	idx := newIndex()
	e := Entry{
		CacheDir: services.HFHubCacheDirName,
		Family:   services.CacheFamilyHFHub,
		Model:    "org/repo",
		Files:    []string{"models--org--repo"},
	}

	if _, ok := idx.Verify(e, root); !ok {
		t.Fatalf("expected the entry to verify while its directory exists")
	}

	if err := os.RemoveAll(filepath.Join(root, services.HFHubCacheDirName, "hub", "models--org--repo")); err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Verify(e, root); ok {
		t.Fatalf("expected the entry to report stale after its directory was removed")
	}

	// Native entries have nothing to verify and always report true.
	native := Entry{CacheDir: "ollama", Family: services.CacheFamilyNative, Model: "llama3.1:8b"}
	if _, ok := idx.Verify(native, root); !ok {
		t.Fatalf("expected a native entry to always verify true")
	}
}

func TestLeastRecentlyUsedOrdering(t *testing.T) {
	idx := newIndex()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	// A: recently used (real signal).
	idx.entries[entryKey("huggingface", "a")] = Entry{CacheDir: "huggingface", Model: "a", LastUsed: now}
	// B: used long ago (real signal, oldest known usage -- should be the
	// MOST evictable).
	idx.entries[entryKey("huggingface", "b")] = Entry{CacheDir: "huggingface", Model: "b", LastUsed: now.Add(-30 * 24 * time.Hour)}
	// C: no LastUsed at all (a backfilled entry never touched by the
	// resident-implies-used reconciler), with a moderately old PulledAt.
	// Must NOT be treated as automatically coldest just because it lacks a
	// LastUsed (design doc §8.5 / the #632-inverse rule) -- it should land
	// based on its PulledAt, not jump to the front.
	idx.entries[entryKey("huggingface", "c")] = Entry{CacheDir: "huggingface", Model: "c", PulledAt: now.Add(-5 * 24 * time.Hour)}
	// D: no LastUsed and no PulledAt at all (degenerate/defensive case) --
	// must sort LAST (least evictable), not guessed at.
	idx.entries[entryKey("huggingface", "d")] = Entry{CacheDir: "huggingface", Model: "d"}

	order := idx.LeastRecentlyUsed()
	if len(order) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(order))
	}
	got := make([]string, len(order))
	for i, e := range order {
		got[i] = e.Model
	}
	want := []string{"b", "c", "a", "d"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LeastRecentlyUsed order = %v, want %v", got, want)
		}
	}
}

func TestEntriesByDirAndLookupForEngine(t *testing.T) {
	idx := newIndex()
	idx.entries[entryKey("huggingface", "a")] = Entry{CacheDir: "huggingface", Model: "a", Engine: "vllm"}
	idx.entries[entryKey("huggingface", "b")] = Entry{CacheDir: "huggingface", Model: "b", Engine: "sglang"}
	idx.entries[entryKey("llamacpp", "c.gguf")] = Entry{CacheDir: "llamacpp", Model: "c.gguf", Engine: "llamacpp"}

	byDir := idx.EntriesByDir()
	if len(byDir["huggingface"]) != 2 {
		t.Fatalf("expected 2 huggingface entries, got %d", len(byDir["huggingface"]))
	}
	if byDir["huggingface"][0].Model != "a" || byDir["huggingface"][1].Model != "b" {
		t.Errorf("expected huggingface entries sorted by model, got %+v", byDir["huggingface"])
	}

	if _, ok := idx.LookupForEngine("vllm", "a"); !ok {
		t.Errorf("expected LookupForEngine(vllm, a) to resolve via services.EngineCacheDirs")
	}
	if _, ok := idx.LookupForEngine("vllm", "c.gguf"); ok {
		t.Errorf("vllm's cache dir must not see llamacpp's entry")
	}
	if _, ok := idx.LookupForEngine("no-such-engine", "a"); ok {
		t.Errorf("expected an unknown engine to resolve to nothing")
	}
}

func TestFilesForReturnsACopy(t *testing.T) {
	idx := newIndex()
	idx.entries[entryKey("llamacpp", "m")] = Entry{CacheDir: "llamacpp", Model: "m", Files: []string{"a", "b"}}
	files := idx.FilesFor("llamacpp", "m")
	files[0] = "mutated"
	got, _ := idx.Lookup("llamacpp", "m")
	if got.Files[0] != "a" {
		t.Fatalf("FilesFor must return a copy; mutating it corrupted the index: %+v", got.Files)
	}
}

func TestUpsertRequiresKey(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), FileName), nil)
	if err := s.Upsert(Entry{Model: "no-cache-dir"}); err == nil {
		t.Errorf("expected Upsert to reject an entry with no CacheDir")
	}
	if err := s.Upsert(Entry{CacheDir: "huggingface"}); err == nil {
		t.Errorf("expected Upsert to reject an entry with no Model")
	}
}

// TestWriteSnapshotRefusesAStaleVersion pins citadel #937's fix directly and
// deterministically (no goroutine timing involved): flushIfDue's debounced
// write takes its snapshot under s.mu but writes to disk AFTER releasing it,
// so a concurrent Upsert/Remove that flushes NEWER state (synchronously,
// still under s.mu) can complete and reach disk BEFORE that older snapshot's
// write finally lands. Without a staleness guard, the older write's rename
// would silently revert the newer one on disk. This test manually replays
// that exact interleaving: capture a snapshot+version, mutate further (a
// "someone raced ahead" stand-in), then attempt to writeSnapshot the STALE
// capture and assert it is a no-op that never reaches disk.
func TestWriteSnapshotRefusesAStaleVersion(t *testing.T) {
	dir := t.TempDir()
	s := Open(filepath.Join(dir, FileName), nil)

	if err := s.Upsert(Entry{CacheDir: "huggingface", Model: "org/a", Engine: "vllm", Files: []string{"models--org--a"}}); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	// Simulate flushIfDue: snapshot + version captured under s.mu, exactly
	// like flushIfDue itself does, standing in for a MarkUsed call that
	// hasn't reached disk yet.
	s.mu.Lock()
	staleSnapshot := s.idx.snapshot()
	staleVersion := s.version
	s.mu.Unlock()

	// A second, independent mutation "wins the race" and reaches disk first
	// -- newer state, higher version, synchronous flush under s.mu.
	if err := s.Upsert(Entry{CacheDir: "huggingface", Model: "org/b", Engine: "vllm", Files: []string{"models--org--b"}}); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	// The stale, already-superseded snapshot finally attempts its write --
	// must be refused (a no-op), not a clobber.
	if err := s.writeSnapshot(staleSnapshot, staleVersion); err != nil {
		t.Fatalf("writeSnapshot(stale): %v", err)
	}

	// Load fresh from disk (bypassing the in-memory Store entirely) and
	// confirm the newer entry survived and nothing was reverted.
	reloaded, err := Load(s.path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := reloaded.Lookup("huggingface", "org/a"); !ok {
		t.Errorf("expected the first entry to still be on disk")
	}
	if _, ok := reloaded.Lookup("huggingface", "org/b"); !ok {
		t.Errorf("stale flushIfDue-style write clobbered the newer Upsert entry on disk")
	}
}

// TestConcurrentUpsertAndMarkUsedSurviveOnDisk exercises the real concurrent
// path (Upsert's synchronous flush racing MarkUsed's debounced flushIfDue)
// under go test -race, so the new version/lastWrittenVersion bookkeeping
// itself is proven data-race-free, not just logically correct. Each Upsert's
// flush is synchronous (returns only after its own write completes), so by
// the time all goroutines finish, every upserted entry must be present on a
// fresh disk load regardless of how MarkUsed's debounced writes interleaved.
func TestConcurrentUpsertAndMarkUsedSurviveOnDisk(t *testing.T) {
	dir := t.TempDir()
	s := Open(filepath.Join(dir, FileName), nil)

	if err := s.Upsert(Entry{CacheDir: "llamacpp", Model: "seed.gguf", Engine: "llamacpp", Files: []string{"seed.gguf"}, Source: SourceBackfill}); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	// markUsedFlushMinGap (5s) would otherwise debounce all but the very
	// first MarkUsed call away before it ever takes a snapshot -- a
	// wall-clock test finishes in milliseconds, so flushIfDue's actual
	// write path (the one this fix targets) would barely run. Force every
	// call to see time strictly past the gap, via an atomic counter (s.now
	// is read from multiple goroutines with no lock held), so every
	// MarkUsed genuinely races a flush to disk.
	var clockTick int64
	s.now = func() time.Time {
		tick := atomic.AddInt64(&clockTick, 1)
		return time.Unix(0, 0).Add(time.Duration(tick) * time.Hour)
	}

	const n = 25
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.Upsert(Entry{
				CacheDir: "llamacpp",
				Model:    fmt.Sprintf("model-%d.gguf", i),
				Engine:   "llamacpp",
				Files:    []string{fmt.Sprintf("model-%d.gguf", i)},
				Source:   SourceBackfill,
			})
		}()
		go func() {
			defer wg.Done()
			s.MarkUsed("llamacpp", "seed.gguf", time.Now())
		}()
	}
	wg.Wait()

	reloaded, err := Load(s.path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := reloaded.Lookup("llamacpp", "seed.gguf"); !ok {
		t.Errorf("expected the seed entry to survive concurrent Upsert/MarkUsed")
	}
	for i := 0; i < n; i++ {
		model := fmt.Sprintf("model-%d.gguf", i)
		if _, ok := reloaded.Lookup("llamacpp", model); !ok {
			t.Errorf("expected %s to be present on disk after concurrent flush, but it was missing (a stale write clobbered it)", model)
		}
	}
}

// --- Scan metadata (citadel #682 P3, design doc §9.3) ---

// TestScanMetadataRoundTrip pins that ReconcileScan's scan metadata
// (ScannedAt/Dirs/LegacyHF) survives a full write-then-Load round trip,
// alongside the entries it discovers in the same pass -- the "version 1"
// format design doc §9.3 specifies (additive fields, FormatVersion unchanged).
func TestScanMetadataRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, services.HFHubCacheDirName, "hub", "models--org--repo", "weights.bin"), "0123456789")

	legacyDir := t.TempDir()
	writeFile(t, filepath.Join(legacyDir, "hub", "models--legacy--dup", "weights.bin"), "legacy-bytes")

	storeDir := t.TempDir()
	path := filepath.Join(storeDir, FileName)
	s := Open(path, nil)
	fixedNow := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixedNow }

	opts := ScanOptions{LegacyHFHubDir: filepath.Join(legacyDir, "hub")}
	if err := s.ReconcileScan(root, opts); err != nil {
		t.Fatalf("ReconcileScan: %v", err)
	}

	// In-memory (pre-reload) view first.
	meta := s.Snapshot().Meta()
	if !meta.ScannedAt.Equal(fixedNow) {
		t.Errorf("in-memory ScannedAt = %v, want %v", meta.ScannedAt, fixedNow)
	}
	if meta.LegacyHF == nil || meta.LegacyHF.SizeBytes != int64(len("legacy-bytes")) {
		t.Fatalf("in-memory LegacyHF = %+v, want a populated entry sized %d", meta.LegacyHF, len("legacy-bytes"))
	}
	if meta.LegacyHF.Path != legacyDir {
		t.Errorf("in-memory LegacyHF.Path = %q, want %q (the parent of the probed hub dir)", meta.LegacyHF.Path, legacyDir)
	}
	var hfDirScan *DirScan
	for i := range meta.Dirs {
		if meta.Dirs[i].Dir == services.HFHubCacheDirName {
			hfDirScan = &meta.Dirs[i]
		}
	}
	if hfDirScan == nil {
		t.Fatalf("expected a DirScan for %s, got %+v", services.HFHubCacheDirName, meta.Dirs)
	}
	if hfDirScan.Family != string(services.CacheFamilyHFHub) || hfDirScan.MeasuredBytes != 10 {
		t.Errorf("unexpected hf-hub DirScan: %+v", hfDirScan)
	}

	// Now the on-disk round trip.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rmeta := reloaded.Meta()
	if !rmeta.ScannedAt.Equal(fixedNow) {
		t.Errorf("reloaded ScannedAt = %v, want %v", rmeta.ScannedAt, fixedNow)
	}
	if rmeta.LegacyHF == nil || rmeta.LegacyHF.SizeBytes != meta.LegacyHF.SizeBytes || rmeta.LegacyHF.Path != meta.LegacyHF.Path {
		t.Errorf("reloaded LegacyHF = %+v, want it to match the in-memory value %+v", rmeta.LegacyHF, meta.LegacyHF)
	}
	if len(rmeta.Dirs) != len(meta.Dirs) {
		t.Fatalf("reloaded Dirs = %+v, want %d entries like the in-memory value", rmeta.Dirs, len(meta.Dirs))
	}
	if _, ok := reloaded.Lookup(services.HFHubCacheDirName, "org/repo"); !ok {
		t.Errorf("expected the discovered entry to ALSO survive the round trip alongside the scan metadata")
	}
}

// TestLoadOldFormatWithoutScanMetadataForwardCompat pins design doc §9.3's
// explicit contract: a file written before scan metadata existed (only
// {"version":1,"entries":[...]}, no scanned_at/dirs/legacy_hf_cache keys at
// all) must still load its entries correctly, with Meta() degrading to its
// zero value ("never scanned") rather than any read failing. This is the
// literal forward-compat case the design doc's "FormatVersion stays 1 --
// additive" decision exists to guarantee.
func TestLoadOldFormatWithoutScanMetadataForwardCompat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	oldFormat := `{
		"version": 1,
		"entries": [
			{
				"cache_dir": "huggingface",
				"family": "hf-hub",
				"model": "meta-llama/Llama-3.1-8B-Instruct",
				"engine": "vllm",
				"files": ["models--meta-llama--Llama-3.1-8B-Instruct"],
				"size_bytes": 16800000000,
				"pulled_at": "2026-08-20T10:00:00Z",
				"last_used": "2026-08-27T14:32:00Z",
				"source": "pull"
			}
		]
	}`
	if err := os.WriteFile(path, []byte(oldFormat), 0o644); err != nil {
		t.Fatal(err)
	}

	idx, err := Load(path)
	if err != nil {
		t.Fatalf("Load of a pre-scan-metadata file must not error, got %v", err)
	}

	e, ok := idx.Lookup(services.HFHubCacheDirName, "meta-llama/Llama-3.1-8B-Instruct")
	if !ok {
		t.Fatalf("expected the pre-existing entry to load correctly")
	}
	if e.SizeBytes != 16_800_000_000 || e.Source != SourcePull {
		t.Errorf("unexpected entry from old-format file: %+v", e)
	}

	meta := idx.Meta()
	if !meta.ScannedAt.IsZero() {
		t.Errorf("ScannedAt = %v, want zero (never scanned) for a file with no scan metadata", meta.ScannedAt)
	}
	if meta.Dirs != nil {
		t.Errorf("Dirs = %+v, want nil for a file with no scan metadata", meta.Dirs)
	}
	if meta.LegacyHF != nil {
		t.Errorf("LegacyHF = %+v, want nil for a file with no scan metadata", meta.LegacyHF)
	}

	// A subsequent write from THIS (upgraded) binary must not choke on
	// having read an old-format file -- round trip it back out and in.
	s := Open(path, nil)
	if got := s.Snapshot().Meta(); !got.ScannedAt.IsZero() {
		t.Errorf("Store opened against an old-format file must also see zero-value Meta, got %+v", got)
	}
	if err := s.Upsert(Entry{CacheDir: "llamacpp", Model: "new.gguf", Engine: "llamacpp", Files: []string{"new.gguf"}}); err != nil {
		t.Fatalf("Upsert after opening an old-format file: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after upgrading an old-format file: %v", err)
	}
	if _, ok := reloaded.Lookup(services.HFHubCacheDirName, "meta-llama/Llama-3.1-8B-Instruct"); !ok {
		t.Errorf("expected the original pre-existing entry to survive the upgrade write")
	}
	if _, ok := reloaded.Lookup("llamacpp", "new.gguf"); !ok {
		t.Errorf("expected the newly-upserted entry to be present after the upgrade write")
	}
}

// TestReconcileScanOmitsUnscannedDirsFromMeasuredTotals pins Meta.Dirs' "no
// row means unmeasured, never zero bytes" rule: a cache_dir this pass never
// found on disk gets no DirScan entry at all.
func TestReconcileScanOmitsUnscannedDirsFromMeasuredTotals(t *testing.T) {
	root := t.TempDir() // nothing on disk under any EngineCacheDirs subdir
	s := Open(filepath.Join(t.TempDir(), FileName), nil)
	if err := s.ReconcileScan(root, ScanOptions{}); err != nil {
		t.Fatalf("ReconcileScan: %v", err)
	}
	meta := s.Snapshot().Meta()
	if len(meta.Dirs) != 0 {
		t.Errorf("Dirs = %+v, want empty when nothing on disk was scannable", meta.Dirs)
	}
	if meta.ScannedAt.IsZero() {
		t.Errorf("ScannedAt must still be set even when nothing was found -- a scan DID run")
	}
}

// TestProbeLegacyHFCacheEmptyAndZeroSize pins probeLegacyHFCache's two
// nil-returning cases: an empty path (caller chose not to probe) and a path
// that sizes to zero (nothing worth reporting).
func TestProbeLegacyHFCacheEmptyAndZeroSize(t *testing.T) {
	if got := probeLegacyHFCache(""); got != nil {
		t.Errorf("probeLegacyHFCache(\"\") = %+v, want nil", got)
	}
	empty := t.TempDir()
	if got := probeLegacyHFCache(empty); got != nil {
		t.Errorf("probeLegacyHFCache(empty dir) = %+v, want nil (nothing to report)", got)
	}
}

// TestIndexedBytesByDir pins the P3 reporting helper's sum-per-dir contract.
func TestIndexedBytesByDir(t *testing.T) {
	dir := t.TempDir()
	s := Open(filepath.Join(dir, FileName), nil)
	if err := s.Upsert(Entry{CacheDir: "huggingface", Model: "a/b", SizeBytes: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(Entry{CacheDir: "huggingface", Model: "c/d", SizeBytes: 200}); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(Entry{CacheDir: "llamacpp", Model: "e.gguf", SizeBytes: 50}); err != nil {
		t.Fatal(err)
	}
	got := s.Snapshot().IndexedBytesByDir()
	if got["huggingface"] != 300 {
		t.Errorf("IndexedBytesByDir()[huggingface] = %d, want 300", got["huggingface"])
	}
	if got["llamacpp"] != 50 {
		t.Errorf("IndexedBytesByDir()[llamacpp] = %d, want 50", got["llamacpp"])
	}
}
