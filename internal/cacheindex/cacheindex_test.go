package cacheindex

import (
	"os"
	"path/filepath"
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
