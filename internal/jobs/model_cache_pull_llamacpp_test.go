// internal/jobs/model_cache_pull_llamacpp_test.go
//
// citadel #906 / #682 P1: llamacpp's MODEL_CACHE_PULL/MODEL_CACHE_EVICT must
// write to and read from ~/citadel-cache/llamacpp in the RAW GGUF layout, not
// the HuggingFace hub-cache layout pullHuggingFace/evictHuggingFace use for
// vllm. These tests are hermetic: no real `hf` binary invocation and no
// network access.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// TestLlamaCppCacheDirMatchesComposeMount guards the pull-serve coherence
// contract, mirroring TestBonsaiCacheDirMatchesComposeMount: the download dir
// must be ~/citadel-cache/llamacpp, exactly what services/compose/llamacpp.yml
// mounts at /models.
func TestLlamaCppCacheDirMatchesComposeMount(t *testing.T) {
	dir := defaultLlamaCppCacheDir()
	want := filepath.Join("citadel-cache", services.LlamaCppCacheDirName)
	if !strings.HasSuffix(dir, want) {
		t.Errorf("defaultLlamaCppCacheDir() = %q, want it to end with %q (the compose mount)", dir, want)
	}
}

// TestBuildLlamaCppGGUFDownloadCommand pins the exact `hf download` argv for
// a llamacpp pull: repo, --local-dir pointed at the raw-GGUF cache dir (NOT
// the HF hub cache), and no bare filename argument (a repo-level pull, unlike
// bonsai's single-file pull).
func TestBuildLlamaCppGGUFDownloadCommand(t *testing.T) {
	localDir := "/home/tester/citadel-cache/llamacpp"
	cmd := BuildLlamaCppGGUFDownloadCommand("hf", "TheBloke/Llama-2-7B-GGUF", localDir, nil, nil)

	args := cmd.Args
	if args[0] != "hf" {
		t.Errorf("expected binary %q, got %q", "hf", args[0])
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"download", "TheBloke/Llama-2-7B-GGUF", "--local-dir", localDir} {
		if !strings.Contains(joined, want) {
			t.Errorf("llamacpp download command missing %q; got: %s", want, joined)
		}
	}
	// Must NOT be a single-file pull: no bare filename between the repo id
	// and --local-dir (that's what forces a full-repo, --include/--exclude
	// filtered download instead).
	for i, a := range args {
		if a == "TheBloke/Llama-2-7B-GGUF" && i+1 < len(args) && args[i+1] != "--local-dir" {
			t.Errorf("expected --local-dir immediately after the repo id (no bare filename), got args=%v", args)
		}
	}
}

// TestBuildLlamaCppGGUFDownloadCommandThreadsPatterns pins that allow/ignore
// patterns from the payload (#828) reach the argv unchanged, so an operator
// or the backend can target a specific quantization without pulling every
// sibling GGUF in the repo.
func TestBuildLlamaCppGGUFDownloadCommandThreadsPatterns(t *testing.T) {
	cmd := BuildLlamaCppGGUFDownloadCommand("hf", "TheBloke/Llama-2-7B-GGUF", "/tmp/llamacpp",
		[]string{"*Q4_K_M.gguf"}, []string{"*.md"})
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--include *Q4_K_M.gguf", "--exclude *.md"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %v missing %q", cmd.Args, want)
		}
	}
}

// TestDirTotalSize pins the before/after no-op-detection primitive: sums
// regular file sizes recursively, and degrades to 0 (not an error) for a
// directory that does not exist yet -- the normal state before a cache dir's
// first-ever pull.
func TestDirTotalSize(t *testing.T) {
	t.Run("nonexistent dir is 0, not an error", func(t *testing.T) {
		if got := dirTotalSize(filepath.Join(t.TempDir(), "does-not-exist")); got != 0 {
			t.Errorf("dirTotalSize(nonexistent) = %d, want 0", got)
		}
	})

	t.Run("sums nested regular files", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.gguf"), make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
		sub := filepath.Join(dir, "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "b.gguf"), make([]byte, 250), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := dirTotalSize(dir); got != 350 {
			t.Errorf("dirTotalSize(dir) = %d, want 350", got)
		}
	})
}

// TestLlamaCppPullSucceeded pins the citadel #906 PR-review BLOCKING fix
// directly: a zero-delta pull (a legitimate no-op redeploy of an
// already-cached GGUF repo, since `hf download` is idempotent and
// MODEL_CACHE_PULL is dispatched on every deploy) must be treated as
// success, not misreported as the #566 CLI-no-op failure -- the gate is
// "is anything there at all", not "did this pull add anything".
func TestLlamaCppPullSucceeded(t *testing.T) {
	tests := []struct {
		name       string
		before     int64
		after      int64
		wantOK     bool
		wantSize   int64
		wantReason string
	}{
		{
			name:       "first pull into an empty dir that stays empty is a real #566 no-op failure",
			before:     0,
			after:      0,
			wantOK:     false,
			wantReason: "after == 0",
		},
		{
			name:     "first-ever pull that lands real bytes succeeds",
			before:   0,
			after:    4 << 30,
			wantOK:   true,
			wantSize: 4 << 30,
		},
		{
			name:     "redeploy of an already-cached repo: zero delta, but after > 0, must succeed",
			before:   4 << 30,
			after:    4 << 30,
			wantOK:   true,
			wantSize: 0,
		},
		{
			name:     "a genuine additional download on top of existing files succeeds with the real delta",
			before:   4 << 30,
			after:    9 << 30,
			wantOK:   true,
			wantSize: 5 << 30,
		},
		{
			name:     "after < before (e.g. a concurrent eviction mid-pull) clamps size to after, never negative",
			before:   9 << 30,
			after:    4 << 30,
			wantOK:   true,
			wantSize: 4 << 30,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size, ok := llamaCppPullSucceeded(tt.before, tt.after)
			if ok != tt.wantOK {
				t.Fatalf("llamaCppPullSucceeded(%d, %d) ok = %v, want %v (%s)", tt.before, tt.after, ok, tt.wantOK, tt.wantReason)
			}
			if ok && size != tt.wantSize {
				t.Errorf("llamaCppPullSucceeded(%d, %d) size = %d, want %d", tt.before, tt.after, size, tt.wantSize)
			}
		})
	}
}

// TestRunGGUFDiskPreflightInjected mirrors TestRunDiskPreflightInjected but
// for llamacpp's GGUF path: same free-space gate, injected metadata/disk
// funcs, no real disk or network access.
func TestRunGGUFDiskPreflightInjected(t *testing.T) {
	origTree, origDisk := hfRepoTreeFn, availableDiskBytesFn
	t.Cleanup(func() { hfRepoTreeFn, availableDiskBytesFn = origTree, origDisk })

	jc := JobContext{}

	t.Run("blocks and downloads nothing when the estimate exceeds free space", func(t *testing.T) {
		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return []hfTreeEntry{
				{Type: "file", Path: "model-Q8_0.gguf", Size: 40 << 30},
			}, nil
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			return 10 << 30, nil
		}

		allow, ignore, err := runGGUFDiskPreflight(jc, "TheBloke/Llama-2-7B-GGUF", nil, nil, diskSafetyMarginBytes)
		if err == nil {
			t.Fatal("expected a blocking error when the estimate exceeds free space")
		}
		if allow != nil || ignore != nil {
			t.Errorf("expected nil patterns on block, got allow=%v ignore=%v", allow, ignore)
		}
		if !strings.Contains(err.Error(), "insufficient disk space") {
			t.Errorf("error = %v, want an 'insufficient disk space' message", err)
		}
	})

	t.Run("proceeds when the estimate fits", func(t *testing.T) {
		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return []hfTreeEntry{
				{Type: "file", Path: "model-Q4_K_M.gguf", Size: 4 << 30},
			}, nil
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			return 100 << 30, nil
		}

		_, _, err := runGGUFDiskPreflight(jc, "TheBloke/Llama-2-7B-GGUF", nil, nil, diskSafetyMarginBytes)
		if err != nil {
			t.Fatalf("expected no error when the estimate fits, got %v", err)
		}
	})

	t.Run("fails open (proceeds, no error) when the metadata fetch errors", func(t *testing.T) {
		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return nil, errors.New("connection reset")
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			t.Error("availableDiskBytesFn must not be called when the metadata fetch already failed")
			return 0, nil
		}

		allow, ignore, err := runGGUFDiskPreflight(jc, "some/gguf-repo", []string{"a/*"}, []string{"b/*"}, diskSafetyMarginBytes)
		if err != nil {
			t.Fatalf("expected fail-open (nil error) on metadata fetch failure, got %v", err)
		}
		if !equalStrs(allow, []string{"a/*"}) || !equalStrs(ignore, []string{"b/*"}) {
			t.Errorf("expected caller patterns to pass through unchanged, got allow=%v ignore=%v", allow, ignore)
		}
	})

	t.Run("fails open (proceeds, no error) when the disk-free read errors", func(t *testing.T) {
		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return []hfTreeEntry{{Type: "file", Path: "model.gguf", Size: 4 << 30}}, nil
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			return 0, errors.New("statfs not supported")
		}

		_, _, err := runGGUFDiskPreflight(jc, "some/gguf-repo", nil, nil, diskSafetyMarginBytes)
		if err != nil {
			t.Fatalf("expected fail-open (nil error) on disk-free read failure, got %v", err)
		}
	})

	t.Run("gates on the full repo size when nothing is cached locally", func(t *testing.T) {
		origDirFn := llamaCppCacheDirFn
		llamaCppCacheDirFn = func() string { return t.TempDir() } // empty: nothing to credit
		t.Cleanup(func() { llamaCppCacheDirFn = origDirFn })

		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return []hfTreeEntry{{Type: "file", Path: "model.gguf", Size: 8 << 30}}, nil
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			return 1 << 30, nil // 1GiB free -- far under the 8GB requirement
		}

		_, _, err := runGGUFDiskPreflight(jc, "some/gguf-repo", nil, nil, diskSafetyMarginBytes)
		if err == nil {
			t.Fatal("expected a blocking error: nothing is cached locally, so the full 8GB is gated on")
		}
	})

	// citadel #906 PR review (BLOCKING): a redeploy of an already-cached GGUF
	// repo on a disk-tight node (tight BECAUSE it's cached) must not fail
	// closed on the full repo size -- the same regression #840's review
	// caught and fixed for the HF-hub path. alreadyCachedGGUFBytes closes
	// this for the GGUF path via an exact repo-relative-path match, without
	// needing the P2 durable index.
	t.Run("nets bytes already present locally, matched by repo-relative path", func(t *testing.T) {
		dir := t.TempDir()
		origDirFn := llamaCppCacheDirFn
		llamaCppCacheDirFn = func() string { return dir }
		t.Cleanup(func() { llamaCppCacheDirFn = origDirFn })

		// Seed a file matching the repo entry's own path/size -- exactly what
		// a prior --local-dir pull of this same repo would have left behind.
		if err := os.WriteFile(filepath.Join(dir, "model.gguf"), make([]byte, 8<<20), 0o644); err != nil {
			t.Fatal(err)
		}

		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return []hfTreeEntry{{Type: "file", Path: "model.gguf", Size: 8 << 30}}, nil
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			t.Error("availableDiskBytesFn must not be called once the cached credit brings requiredBytes to 0")
			return 0, nil
		}

		_, _, err := runGGUFDiskPreflight(jc, "some/gguf-repo", nil, nil, diskSafetyMarginBytes)
		if err != nil {
			t.Fatalf("expected the already-cached file to be credited and the preflight to proceed, got: %v", err)
		}
	})

	t.Run("a file present under an unrelated name is not credited (no false-positive netting)", func(t *testing.T) {
		dir := t.TempDir()
		origDirFn := llamaCppCacheDirFn
		llamaCppCacheDirFn = func() string { return dir }
		t.Cleanup(func() { llamaCppCacheDirFn = origDirFn })

		// A GGUF exists in the dir, but under a DIFFERENT name than the repo
		// entry -- must not be credited (see alreadyCachedGGUFBytes's doc
		// comment on why an under-credit here is the safe direction).
		if err := os.WriteFile(filepath.Join(dir, "some-other-model.gguf"), make([]byte, 8<<20), 0o644); err != nil {
			t.Fatal(err)
		}

		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return []hfTreeEntry{{Type: "file", Path: "model.gguf", Size: 8 << 30}}, nil
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			return 1 << 30, nil
		}

		_, _, err := runGGUFDiskPreflight(jc, "some/gguf-repo", nil, nil, diskSafetyMarginBytes)
		if err == nil {
			t.Fatal("expected a blocking error: the on-disk file's name does not match the repo entry, so it must not be credited")
		}
	})
}

// TestEvictLlamaCppGGUF exercises the eviction path end-to-end against a
// real (but throwaway, t.TempDir()) directory -- no subprocess, no network,
// so this is safe to run anywhere.
func TestEvictLlamaCppGGUF(t *testing.T) {
	orig := llamaCppCacheDirFn
	t.Cleanup(func() { llamaCppCacheDirFn = orig })

	t.Run("removes an exact cached filename", func(t *testing.T) {
		dir := t.TempDir()
		llamaCppCacheDirFn = func() string { return dir }

		ggufPath := filepath.Join(dir, "model-Q4_K_M.gguf")
		if err := os.WriteFile(ggufPath, []byte("fake gguf bytes"), 0o644); err != nil {
			t.Fatal(err)
		}

		h := &ModelCacheEvictHandler{}
		out, err := h.evictLlamaCppGGUF(JobContext{}, "job-906", "model-Q4_K_M.gguf")
		if err != nil {
			t.Fatalf("expected eviction to succeed, got: %v", err)
		}
		if _, statErr := os.Stat(ggufPath); !os.IsNotExist(statErr) {
			t.Errorf("expected %s to be removed, stat err = %v", ggufPath, statErr)
		}
		var result modelCacheEvictResult
		if jsonErr := json.Unmarshal(out, &result); jsonErr != nil {
			t.Fatalf("failed to unmarshal result: %v", jsonErr)
		}
		if result.Status != "evicted" || result.Engine != "llamacpp" {
			t.Errorf("unexpected result: %+v", result)
		}
	})

	t.Run("errors on a repo id with no matching cached filename, rather than guessing", func(t *testing.T) {
		dir := t.TempDir()
		llamaCppCacheDirFn = func() string { return dir }

		h := &ModelCacheEvictHandler{}
		_, err := h.evictLlamaCppGGUF(JobContext{}, "job-906", "TheBloke/Llama-2-7B-GGUF")
		if err == nil {
			t.Fatal("expected an error for a model name with no matching cached file")
		}
	})

	t.Run("does not escape the cache dir on a path-traversal-shaped model name", func(t *testing.T) {
		dir := t.TempDir()
		llamaCppCacheDirFn = func() string { return dir }
		// A real file OUTSIDE dir a naive filepath.Join could otherwise reach.
		outside := filepath.Join(filepath.Dir(dir), "outside.gguf")
		if err := os.WriteFile(outside, []byte("must not be touched"), 0o644); err == nil {
			t.Cleanup(func() { os.Remove(outside) })
		}

		h := &ModelCacheEvictHandler{}
		// filepath.Base already collapses "../outside.gguf" down to the bare
		// filename "outside.gguf" before this ever reaches the explicit
		// same-directory guard, so this resolves to dir/outside.gguf (which
		// does not exist) rather than the real file above -- fails via the
		// ordinary "not found" path, not the guard, but the safety property
		// (never touches the file outside dir) is what this test pins.
		_, err := h.evictLlamaCppGGUF(JobContext{}, "job-906", "../outside.gguf")
		if err == nil {
			t.Fatal("expected an error: no file named outside.gguf exists inside the cache dir")
		}
		if _, statErr := os.Stat(outside); statErr != nil {
			t.Errorf("the outside file must be untouched, stat err = %v", statErr)
		}
	})
}
