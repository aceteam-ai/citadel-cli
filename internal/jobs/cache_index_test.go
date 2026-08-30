package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/cacheindex"
	"github.com/aceteam-ai/citadel-cli/services"
)

// withTestCacheIndex points cacheIndexFn at a throwaway Store rooted in a
// t.TempDir(), restoring the original (nil-by-default, per cache_index.go's
// package doc) seam on cleanup so this test's index never touches the real
// node config dir and other tests never see a leftover override.
func withTestCacheIndex(t *testing.T) *cacheindex.Store {
	t.Helper()
	orig := cacheIndexFn
	store := cacheindex.Open(filepath.Join(t.TempDir(), cacheindex.FileName), nil)
	cacheIndexFn = func() *cacheindex.Store { return store }
	t.Cleanup(func() { cacheIndexFn = orig })
	return store
}

// TestCacheIndexFnDefaultsToNil pins the safety property the whole package
// doc comment in cache_index.go exists to guarantee: absent an explicit
// InitCacheIndexStore call (made only by cmd/work.go's runWork in
// production), cacheIndexFn must return nil -- never a lazily-constructed
// real Store pointed at this machine's actual node config dir. This is what
// keeps every OTHER test in this package (many of which exercise a real
// pull/evict success path) side-effect-free without needing to know this
// feature exists.
func TestCacheIndexFnDefaultsToNil(t *testing.T) {
	if got := cacheIndexFn(); got != nil {
		t.Fatalf("expected cacheIndexFn() to default to nil, got %v -- this would write to the real node config dir from ordinary tests", got)
	}
}

// TestUpsertCacheIndexEntry_NilStoreIsSilentNoOp exercises the actual
// call-site helper (not just the seam default) with no store configured: it
// must neither panic nor log a "warn" (there is nothing to warn about --
// the index is simply unavailable, same as before this feature existed).
func TestUpsertCacheIndexEntry_NilStoreIsSilentNoOp(t *testing.T) {
	var warned bool
	ctx := JobContext{LogFn: func(level, msg string) {
		if level == "warn" {
			warned = true
		}
	}}
	upsertCacheIndexEntry(ctx, "job-1", cacheindex.Entry{CacheDir: "huggingface", Model: "org/repo"})
	if warned {
		t.Errorf("expected no warning when the cache index was never initialized")
	}
}

func TestPullOllama_WritesCacheIndexEntry(t *testing.T) {
	store := withTestCacheIndex(t)
	_, write := fakeBinDir(t)
	write("ollama", `if [ "$1" = "list" ]; then echo "llama3.2:7b  abc123  4.1 GB  3 days ago"; exit 0; fi
exit 0`)

	h := &ModelCachePullHandler{}
	if _, err := h.pullOllama(JobContext{}, "job-1", "llama3.2:7b"); err != nil {
		t.Fatalf("pullOllama: %v", err)
	}

	e, ok := store.Snapshot().Lookup(services.EngineCacheDirs["ollama"].Dir, "llama3.2:7b")
	if !ok {
		t.Fatalf("expected a cache index entry for the pulled ollama model")
	}
	if e.Engine != "ollama" || e.Family != services.CacheFamilyNative {
		t.Errorf("unexpected entry: %+v", e)
	}
	if e.SizeBytes <= 0 {
		t.Errorf("expected a nonzero size parsed from `ollama list`, got %d", e.SizeBytes)
	}
}

func TestEnsureOllamaModel_WritesCacheIndexEntry(t *testing.T) {
	store := withTestCacheIndex(t)
	_, write := fakeBinDir(t)
	write("ollama", `exit 0`)

	if err := ensureOllamaModel(JobContext{}, "qwen2.5:7b", false); err != nil {
		t.Fatalf("ensureOllamaModel: %v", err)
	}

	if _, ok := store.Snapshot().Lookup(services.EngineCacheDirs["ollama"].Dir, "qwen2.5:7b"); !ok {
		t.Fatalf("expected ensureOllamaModel to write a cache index entry -- the #543 SERVICE_START native-ollama pull is a documented write site (design doc §8.3) distinct from MODEL_CACHE_PULL")
	}
}

func TestEvictOllama_RemovesCacheIndexEntry(t *testing.T) {
	store := withTestCacheIndex(t)
	if err := store.Upsert(cacheindex.Entry{
		CacheDir: services.EngineCacheDirs["ollama"].Dir,
		Family:   services.CacheFamilyNative,
		Model:    "llama3.2:7b",
		Engine:   "ollama",
	}); err != nil {
		t.Fatal(err)
	}

	_, write := fakeBinDir(t)
	write("ollama", `exit 0`)

	h := &ModelCacheEvictHandler{}
	if _, err := h.evictOllama(JobContext{}, "job-1", "llama3.2:7b"); err != nil {
		t.Fatalf("evictOllama: %v", err)
	}

	if _, ok := store.Snapshot().Lookup(services.EngineCacheDirs["ollama"].Dir, "llama3.2:7b"); ok {
		t.Fatalf("expected the cache index entry to be removed after eviction")
	}
}

func TestEvictHuggingFace_RemovesCacheIndexEntry(t *testing.T) {
	store := withTestCacheIndex(t)

	hubDir := t.TempDir()
	t.Setenv("HF_HUB_CACHE", hubDir)
	modelDir := filepath.Join(hubDir, "models--org--repo")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "weights.bin"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := store.Upsert(cacheindex.Entry{
		CacheDir: services.HFHubCacheDirName,
		Family:   services.CacheFamilyHFHub,
		Model:    "org/repo",
		Engine:   "vllm",
		Files:    []string{"models--org--repo"},
	}); err != nil {
		t.Fatal(err)
	}

	h := &ModelCacheEvictHandler{}
	if _, err := h.evictHuggingFace(JobContext{}, "job-1", "org/repo", "vllm"); err != nil {
		t.Fatalf("evictHuggingFace: %v", err)
	}

	if _, statErr := os.Stat(modelDir); !os.IsNotExist(statErr) {
		t.Errorf("expected the on-disk cache dir to be removed, stat err = %v", statErr)
	}
	if _, ok := store.Snapshot().Lookup(services.HFHubCacheDirName, "org/repo"); ok {
		t.Fatalf("expected the cache index entry to be removed after eviction")
	}
}

func TestEvictLlamaCppGGUF_UpdatesCacheIndex(t *testing.T) {
	origDirFn := llamaCppCacheDirFn
	t.Cleanup(func() { llamaCppCacheDirFn = origDirFn })

	t.Run("removing the last file drops the whole entry", func(t *testing.T) {
		store := withTestCacheIndex(t)
		dir := t.TempDir()
		llamaCppCacheDirFn = func() string { return dir }

		if err := os.WriteFile(filepath.Join(dir, "model.gguf"), []byte("gguf"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := store.Upsert(cacheindex.Entry{
			CacheDir: services.LlamaCppCacheDirName,
			Family:   services.CacheFamilyGGUFDir,
			Model:    "TheBloke/Some-GGUF",
			Engine:   "llamacpp",
			Files:    []string{"model.gguf"},
		}); err != nil {
			t.Fatal(err)
		}

		h := &ModelCacheEvictHandler{}
		if _, err := h.evictLlamaCppGGUF(JobContext{}, "job-1", "model.gguf"); err != nil {
			t.Fatalf("evictLlamaCppGGUF: %v", err)
		}

		if _, ok := store.Snapshot().Lookup(services.LlamaCppCacheDirName, "TheBloke/Some-GGUF"); ok {
			t.Fatalf("expected the entry to be dropped once its only file was removed")
		}
	})

	t.Run("removing one of several files only trims that file", func(t *testing.T) {
		store := withTestCacheIndex(t)
		dir := t.TempDir()
		llamaCppCacheDirFn = func() string { return dir }

		if err := os.WriteFile(filepath.Join(dir, "a.gguf"), []byte("a"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "b.gguf"), []byte("b"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := store.Upsert(cacheindex.Entry{
			CacheDir: services.LlamaCppCacheDirName,
			Family:   services.CacheFamilyGGUFDir,
			Model:    "TheBloke/Multi-GGUF",
			Engine:   "llamacpp",
			Files:    []string{"a.gguf", "b.gguf"},
		}); err != nil {
			t.Fatal(err)
		}

		h := &ModelCacheEvictHandler{}
		if _, err := h.evictLlamaCppGGUF(JobContext{}, "job-1", "a.gguf"); err != nil {
			t.Fatalf("evictLlamaCppGGUF: %v", err)
		}

		e, ok := store.Snapshot().Lookup(services.LlamaCppCacheDirName, "TheBloke/Multi-GGUF")
		if !ok {
			t.Fatalf("expected the entry to survive with its remaining file")
		}
		if len(e.Files) != 1 || e.Files[0] != "b.gguf" {
			t.Errorf("expected only b.gguf to remain, got %+v", e.Files)
		}
	})

	t.Run("evicting a file with no index entry is a harmless no-op", func(t *testing.T) {
		withTestCacheIndex(t)
		dir := t.TempDir()
		llamaCppCacheDirFn = func() string { return dir }
		if err := os.WriteFile(filepath.Join(dir, "untracked.gguf"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		h := &ModelCacheEvictHandler{}
		if _, err := h.evictLlamaCppGGUF(JobContext{}, "job-1", "untracked.gguf"); err != nil {
			t.Fatalf("evictLlamaCppGGUF should still succeed on the filesystem op: %v", err)
		}
	})
}

// TestPullBonsai_UpsertShapeMatchesDesign pins the exact Entry shape
// pullBonsai's cache-index write uses (design doc §8.1's "bonsai records
// its one fixed file" case). bonsaiCacheDir() has no injectable test seam
// (unlike llamaCppCacheDirFn), so a full pullBonsai() run would write into
// this machine's real ~/citadel-cache/bonsai -- this test instead exercises
// the write helper directly with the identical Entry pullBonsai constructs,
// which is what actually matters here (that the shape/keys are right and
// the write reaches the configured store).
func TestPullBonsai_UpsertShapeMatchesDesign(t *testing.T) {
	store := withTestCacheIndex(t)
	upsertCacheIndexEntry(JobContext{}, "job-1", cacheindex.Entry{
		CacheDir:  services.BonsaiCacheDirName,
		Family:    services.CacheFamilyGGUFDir,
		Model:     bonsaiGGUFFile,
		Engine:    "bonsai",
		Files:     []string{bonsaiGGUFFile},
		SizeBytes: 12345,
	})

	e, ok := store.Snapshot().Lookup(services.BonsaiCacheDirName, bonsaiGGUFFile)
	if !ok || e.Engine != "bonsai" || e.SizeBytes != 12345 || e.Family != services.CacheFamilyGGUFDir {
		t.Fatalf("unexpected bonsai cache index entry: %+v (ok=%v)", e, ok)
	}
	if len(e.Files) != 1 || e.Files[0] != bonsaiGGUFFile {
		t.Errorf("expected Files=[%s], got %v", bonsaiGGUFFile, e.Files)
	}
}

// TestRecordLlamaCppCacheIndexEntry_TreeFetchSucceeds pins design doc §8.1's
// "post-pull intersection" rule: the entry's Files must be the tree entries
// that pass the pull's own allow/ignore patterns AND actually exist on disk
// -- the COMPLETE current set, not just what this particular pull call
// happened to newly download (so a no-op redeploy still records accurate,
// complete provenance).
func TestRecordLlamaCppCacheIndexEntry_TreeFetchSucceeds(t *testing.T) {
	store := withTestCacheIndex(t)
	dir := t.TempDir()

	// On disk: two of the three tree entries. The third (extra.gguf) is in
	// the tree but was never downloaded (e.g. filtered out) -- must NOT
	// appear in Files.
	if err := os.WriteFile(filepath.Join(dir, "model.Q4_K_M.gguf"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	origTree := hfRepoTreeFn
	t.Cleanup(func() { hfRepoTreeFn = origTree })
	hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
		return []hfTreeEntry{
			{Type: "file", Path: "model.Q4_K_M.gguf", Size: 1},
			{Type: "file", Path: "config.json", Size: 2},
			{Type: "file", Path: "extra.gguf", Size: 3}, // not on disk
		}, nil
	}

	recordLlamaCppCacheIndexEntry(JobContext{}, "job-1", "TheBloke/Some-GGUF", nil, nil, dir, map[string]bool{}, 100)

	e, ok := store.Snapshot().Lookup(services.LlamaCppCacheDirName, "TheBloke/Some-GGUF")
	if !ok {
		t.Fatalf("expected an entry to be recorded")
	}
	got := map[string]bool{}
	for _, f := range e.Files {
		got[f] = true
	}
	if !got["model.Q4_K_M.gguf"] || !got["config.json"] || got["extra.gguf"] {
		t.Errorf("unexpected Files: %v", e.Files)
	}
	if e.SizeBytes != 100 {
		t.Errorf("expected SizeBytes=100, got %d", e.SizeBytes)
	}
}

// TestRecordLlamaCppCacheIndexEntry_TreeFetchFailsFallsBackToDiff pins the
// documented degrade path (recordLlamaCppCacheIndexEntry's doc comment):
// when the repo-tree re-fetch fails, the entry is built from a before/after
// directory diff, unioned with whatever Files an existing entry already had
// -- so a transient network hiccup after a successful pull never loses
// previously-known provenance.
func TestRecordLlamaCppCacheIndexEntry_TreeFetchFailsFallsBackToDiff(t *testing.T) {
	store := withTestCacheIndex(t)
	dir := t.TempDir()

	// Pre-seed an entry as if a PRIOR pull already recorded "old.gguf".
	if err := store.Upsert(cacheindex.Entry{
		CacheDir: services.LlamaCppCacheDirName,
		Family:   services.CacheFamilyGGUFDir,
		Model:    "TheBloke/Some-GGUF",
		Engine:   "llamacpp",
		Files:    []string{"old.gguf"},
	}); err != nil {
		t.Fatal(err)
	}

	beforeFiles := map[string]bool{"old.gguf": true}
	// New file that landed during THIS pull.
	if err := os.WriteFile(filepath.Join(dir, "old.gguf"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.gguf"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	origTree := hfRepoTreeFn
	t.Cleanup(func() { hfRepoTreeFn = origTree })
	hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
		return nil, errors.New("network unavailable")
	}

	recordLlamaCppCacheIndexEntry(JobContext{}, "job-1", "TheBloke/Some-GGUF", nil, nil, dir, beforeFiles, 200)

	e, ok := store.Snapshot().Lookup(services.LlamaCppCacheDirName, "TheBloke/Some-GGUF")
	if !ok {
		t.Fatalf("expected the entry to still exist after the fallback path")
	}
	got := map[string]bool{}
	for _, f := range e.Files {
		got[f] = true
	}
	if !got["old.gguf"] || !got["new.gguf"] {
		t.Errorf("expected the fallback to union the new file with the pre-existing one, got %v", e.Files)
	}
}
