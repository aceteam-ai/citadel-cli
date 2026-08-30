package cacheindex

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/services"
)

// writeFile is a small test helper that creates dir/name with the given
// content, making parent directories as needed.
func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileScanDiscoversEachFamily(t *testing.T) {
	root := t.TempDir()

	// hf-hub: a models--org--repo directory under huggingface/hub.
	writeFile(t, filepath.Join(root, services.HFHubCacheDirName, "hub", "models--meta-llama--Llama-3.1-8B-Instruct", "snapshots", "abc", "model.safetensors"), "0123456789")

	// gguf-dir: a flat file under llamacpp/ and one under bonsai/.
	writeFile(t, filepath.Join(root, services.LlamaCppCacheDirName, "llama-2-7b.Q4_K_M.gguf"), "gguf-bytes")
	writeFile(t, filepath.Join(root, services.BonsaiCacheDirName, "Bonsai-27B-Q1_0.gguf"), "bonsai-bytes")

	// native: an ollama store (must be skipped by design) and a tei store
	// (must get an aggregate "_store" row).
	writeFile(t, filepath.Join(root, "ollama", "models", "blobs", "sha256-abc"), "ollama-blob")
	writeFile(t, filepath.Join(root, "tei", "some-file"), "tei-bytes")

	dir := t.TempDir()
	s := Open(filepath.Join(dir, FileName), nil)
	if err := s.ReconcileScan(root); err != nil {
		t.Fatalf("ReconcileScan: %v", err)
	}

	idx := s.Snapshot()

	hf, ok := idx.Lookup(services.HFHubCacheDirName, "meta-llama/Llama-3.1-8B-Instruct")
	if !ok {
		t.Fatalf("expected an hf-hub entry to be backfilled")
	}
	if hf.Source != SourceBackfill || hf.SizeBytes != 10 || hf.Family != services.CacheFamilyHFHub {
		t.Errorf("unexpected hf-hub backfill entry: %+v", hf)
	}
	if hf.PulledAt.IsZero() {
		t.Errorf("expected PulledAt to be set from directory mtime")
	}

	gguf, ok := idx.Lookup(services.LlamaCppCacheDirName, "llama-2-7b.Q4_K_M.gguf")
	if !ok || gguf.Source != SourceBackfill || gguf.Family != services.CacheFamilyGGUFDir {
		t.Fatalf("expected a llamacpp gguf-dir entry keyed by filename, got %+v (ok=%v)", gguf, ok)
	}

	bonsai, ok := idx.Lookup(services.BonsaiCacheDirName, "Bonsai-27B-Q1_0.gguf")
	if !ok || bonsai.Engine != "bonsai" {
		t.Fatalf("expected a bonsai gguf-dir entry, got %+v (ok=%v)", bonsai, ok)
	}

	if _, ok := idx.Lookup("ollama", nativeAggregateModel); ok {
		t.Errorf("ollama must NOT get a synthetic _store aggregate row (it has real per-model tracking at pull/evict time)")
	}

	teiStore, ok := idx.Lookup("tei", nativeAggregateModel)
	if !ok || teiStore.Family != services.CacheFamilyNative {
		t.Fatalf("expected an aggregate _store row for tei, got %+v (ok=%v)", teiStore, ok)
	}
	if teiStore.SizeBytes <= 0 {
		t.Errorf("expected a nonzero aggregate size for tei, got %d", teiStore.SizeBytes)
	}
}

func TestReconcileScanNeverOverwritesAPullEntry(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, services.HFHubCacheDirName, "hub", "models--org--repo", "weights.bin"), "0123456789")

	storeDir := t.TempDir()
	s := Open(filepath.Join(storeDir, FileName), nil)

	pullTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Upsert(Entry{
		CacheDir:  services.HFHubCacheDirName,
		Family:    services.CacheFamilyHFHub,
		Model:     "org/repo",
		Engine:    "vllm",
		Files:     []string{"models--org--repo"},
		SizeBytes: 999999999, // deliberately does not match the on-disk 10 bytes
		PulledAt:  pullTime,
		LastUsed:  pullTime,
		Source:    SourcePull,
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.ReconcileScan(root); err != nil {
		t.Fatalf("ReconcileScan: %v", err)
	}

	e, ok := s.Snapshot().Lookup(services.HFHubCacheDirName, "org/repo")
	if !ok {
		t.Fatalf("expected the pull entry to survive the scan")
	}
	if e.Source != SourcePull || e.SizeBytes != 999999999 || !e.PulledAt.Equal(pullTime) {
		t.Fatalf("ReconcileScan must never overwrite a SourcePull entry, got %+v", e)
	}
}

func TestReconcileScanRefreshesBackfillEntriesButKeepsLastUsed(t *testing.T) {
	root := t.TempDir()
	dirPath := filepath.Join(root, services.LlamaCppCacheDirName, "model.gguf")
	writeFile(t, dirPath, "aaaaaaaaaa") // 10 bytes

	storeDir := t.TempDir()
	s := Open(filepath.Join(storeDir, FileName), nil)
	if err := s.ReconcileScan(root); err != nil {
		t.Fatalf("first ReconcileScan: %v", err)
	}
	used := time.Now()
	s.MarkUsed(services.LlamaCppCacheDirName, "model.gguf", used)

	// Grow the file, then rescan -- size should refresh, LastUsed should NOT
	// be clobbered back to zero.
	if err := os.WriteFile(dirPath, []byte("aaaaaaaaaaaaaaaaaaaa"), 0o644); err != nil { // 20 bytes
		t.Fatal(err)
	}
	if err := s.ReconcileScan(root); err != nil {
		t.Fatalf("second ReconcileScan: %v", err)
	}

	e, ok := s.Snapshot().Lookup(services.LlamaCppCacheDirName, "model.gguf")
	if !ok {
		t.Fatalf("expected the entry to still exist after rescanning")
	}
	if e.SizeBytes != 20 {
		t.Errorf("expected size to refresh to 20, got %d", e.SizeBytes)
	}
	if !e.LastUsed.Equal(used) {
		t.Errorf("expected LastUsed to survive the rescan untouched, got %v want %v", e.LastUsed, used)
	}
}

func TestReconcileScanDropsStaleEntries(t *testing.T) {
	root := t.TempDir()
	ggufPath := filepath.Join(root, services.LlamaCppCacheDirName, "gone.gguf")
	writeFile(t, ggufPath, "bytes")

	storeDir := t.TempDir()
	s := Open(filepath.Join(storeDir, FileName), nil)
	if err := s.ReconcileScan(root); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if _, ok := s.Snapshot().Lookup(services.LlamaCppCacheDirName, "gone.gguf"); !ok {
		t.Fatalf("expected the entry to exist after the first scan")
	}

	if err := os.Remove(ggufPath); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileScan(root); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if _, ok := s.Snapshot().Lookup(services.LlamaCppCacheDirName, "gone.gguf"); ok {
		t.Fatalf("expected the entry to be dropped once its file was removed out-of-band")
	}
}

// TestReconcileScanPreservesRepoKeyedGGUFPullEntry pins the fix for a real
// bug caught in review: a MODEL_CACHE_PULL for a gguf-dir repo (llamacpp)
// keys its entry by the REPO ID (e.g. "TheBloke/Some-GGUF"), not by any of
// its file names -- a key ReconcileScan's directory walk can never
// reconstruct (scanGGUFDir only ever produces FILENAME keys). A naive
// "prune any index key the scan didn't rediscover" would delete this entry
// on every single `citadel work` restart, destroying the repo-to-files
// provenance the whole gguf-dir Files field exists to carry (and that P6's
// exact-provenance eviction depends on). It must survive, and the scan must
// NOT also create a second, filename-keyed entry double-counting the same
// bytes under a different key.
func TestReconcileScanPreservesRepoKeyedGGUFPullEntry(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, services.LlamaCppCacheDirName, "a.gguf"), "aaaa")
	writeFile(t, filepath.Join(root, services.LlamaCppCacheDirName, "b.gguf"), "bbbb")

	storeDir := t.TempDir()
	s := Open(filepath.Join(storeDir, FileName), nil)
	if err := s.Upsert(Entry{
		CacheDir: services.LlamaCppCacheDirName,
		Family:   services.CacheFamilyGGUFDir,
		Model:    "TheBloke/Some-GGUF",
		Engine:   "llamacpp",
		Files:    []string{"a.gguf", "b.gguf"},
		Source:   SourcePull,
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.ReconcileScan(root); err != nil {
		t.Fatalf("ReconcileScan: %v", err)
	}

	idx := s.Snapshot()
	e, ok := idx.Lookup(services.LlamaCppCacheDirName, "TheBloke/Some-GGUF")
	if !ok {
		t.Fatalf("expected the repo-keyed pull entry to survive the scan")
	}
	if e.Source != SourcePull || len(e.Files) != 2 {
		t.Fatalf("expected the pull entry untouched, got %+v", e)
	}

	// No duplicate filename-keyed entries for files already claimed by the
	// repo-keyed entry above.
	if _, ok := idx.Lookup(services.LlamaCppCacheDirName, "a.gguf"); ok {
		t.Errorf("expected no duplicate entry keyed by a.gguf (already claimed by the repo-keyed pull entry)")
	}
	if _, ok := idx.Lookup(services.LlamaCppCacheDirName, "b.gguf"); ok {
		t.Errorf("expected no duplicate entry keyed by b.gguf (already claimed by the repo-keyed pull entry)")
	}
}

// TestReconcileScanNeverPrunesAnUnscannableDir pins the second half of the
// same review finding: a cache_dir this scan could not read (missing, or --
// in production -- an operator HF_HUB_CACHE/HUGGINGFACE_HUB_CACHE/HF_HOME
// override this scan does not resolve, see ReconcileScan's doc comment)
// must never be treated as "everything in it is gone". Absence of a signal
// (could not scan) must never be read as a negative signal (nothing is
// there) -- the same rule this codebase applies to VRAM/RAM preflights and
// #632's residency floor.
func TestReconcileScanNeverPrunesAnUnscannableDir(t *testing.T) {
	// root deliberately has NO huggingface/hub directory at all (simulating
	// an override, or simply a node that has never pulled anything yet).
	root := t.TempDir()

	storeDir := t.TempDir()
	s := Open(filepath.Join(storeDir, FileName), nil)
	if err := s.Upsert(Entry{
		CacheDir: services.HFHubCacheDirName,
		Family:   services.CacheFamilyHFHub,
		Model:    "org/repo",
		Engine:   "vllm",
		Files:    []string{"models--org--repo"},
		Source:   SourcePull,
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.ReconcileScan(root); err != nil {
		t.Fatalf("ReconcileScan: %v", err)
	}

	if _, ok := s.Snapshot().Lookup(services.HFHubCacheDirName, "org/repo"); !ok {
		t.Fatalf("expected the entry to survive: its cache_dir could not be scanned, so absence must not be treated as staleness")
	}
}

func TestHfDirToModelID(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"models--meta-llama--Llama-3.1-8B-Instruct", "meta-llama/Llama-3.1-8B-Instruct", true},
		{"models--single", "single", true},
		{"not-a-models-dir", "", false},
	}
	for _, c := range cases {
		got, ok := hfDirToModelID(c.in)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("hfDirToModelID(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}
