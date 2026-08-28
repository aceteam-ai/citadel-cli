package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestHFDownloadArgsFilteredBackwardCompat pins that absent patterns produce
// byte-identical argv to the pre-#828 hfDownloadArgs -- the whole point of
// "optional and inert when absent".
func TestHFDownloadArgsFilteredBackwardCompat(t *testing.T) {
	got := hfDownloadArgsFiltered("meta-llama/Llama-2-7b-chat-hf", "", "", nil, nil)
	want := hfDownloadArgs("meta-llama/Llama-2-7b-chat-hf", "", "")
	if !equalStrs(got, want) {
		t.Errorf("hfDownloadArgsFiltered(no patterns) = %v, want %v (identical to hfDownloadArgs)", got, want)
	}
}

// TestHFDownloadArgsFilteredThreadsPatterns asserts the exact argv shape a
// caller-supplied allow/ignore list produces: repeated --include/--exclude
// flags, one per pattern, matching both `hf` and `huggingface-cli`'s grammar.
func TestHFDownloadArgsFilteredThreadsPatterns(t *testing.T) {
	got := hfDownloadArgsFiltered(
		"Lightricks/LTX-Video", "", "",
		[]string{"transformer/*", "vae/*"},
		[]string{"*.bin"},
	)
	want := []string{
		"download", "Lightricks/LTX-Video",
		"--include", "transformer/*",
		"--include", "vae/*",
		"--exclude", "*.bin",
	}
	if !equalStrs(got, want) {
		t.Errorf("hfDownloadArgsFiltered(patterns) = %v, want %v", got, want)
	}
}

func TestBuildHuggingFaceDownloadCommandFiltered(t *testing.T) {
	cmd := BuildHuggingFaceDownloadCommandFiltered("hf", "Lightricks/LTX-Video",
		[]string{"transformer/*", "vae/*", "text_encoder/*", "tokenizer/*", "scheduler/*"}, nil)
	args := cmd.Args
	joined := strings.Join(args, " ")
	for _, want := range []string{"--include transformer/*", "--include vae/*"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %v missing %q", args, want)
		}
	}

	// No patterns must produce the exact same command as the unfiltered builder.
	unfiltered := BuildHuggingFaceDownloadCommand("hf", "meta-llama/Llama-2-7b-chat-hf")
	filtered := BuildHuggingFaceDownloadCommandFiltered("hf", "meta-llama/Llama-2-7b-chat-hf", nil, nil)
	if !equalStrs(unfiltered.Args, filtered.Args) {
		t.Errorf("BuildHuggingFaceDownloadCommandFiltered(no patterns) = %v, want %v", filtered.Args, unfiltered.Args)
	}
}

// TestHFCacheBaseDirPrecedence pins the env-var precedence hfCacheBaseDir must
// follow to match huggingface_hub's own resolution (constants.py): HF_HUB_CACHE
// wins outright, then the legacy HUGGINGFACE_HUB_CACHE, then "$HF_HOME/hub",
// then ~/.cache/huggingface/hub. Getting this wrong means the disk preflight
// measures free space on the wrong volume on exactly the nodes an operator
// bothered to relocate the cache on.
func TestHFCacheBaseDirPrecedence(t *testing.T) {
	t.Run("HF_HUB_CACHE wins outright", func(t *testing.T) {
		t.Setenv("HF_HUB_CACHE", "/mnt/models/hub")
		t.Setenv("HUGGINGFACE_HUB_CACHE", "/other/hub")
		t.Setenv("HF_HOME", "/other/home")
		if got := hfCacheBaseDir(); got != "/mnt/models/hub" {
			t.Errorf("hfCacheBaseDir() = %q, want /mnt/models/hub", got)
		}
	})

	t.Run("legacy HUGGINGFACE_HUB_CACHE wins over HF_HOME", func(t *testing.T) {
		t.Setenv("HF_HUB_CACHE", "")
		t.Setenv("HUGGINGFACE_HUB_CACHE", "/legacy/hub")
		t.Setenv("HF_HOME", "/other/home")
		if got := hfCacheBaseDir(); got != "/legacy/hub" {
			t.Errorf("hfCacheBaseDir() = %q, want /legacy/hub", got)
		}
	})

	t.Run("HF_HOME/hub when neither cache var is set", func(t *testing.T) {
		t.Setenv("HF_HUB_CACHE", "")
		t.Setenv("HUGGINGFACE_HUB_CACHE", "")
		t.Setenv("HF_HOME", "/custom/home")
		want := filepath.Join("/custom/home", "hub")
		if got := hfCacheBaseDir(); got != want {
			t.Errorf("hfCacheBaseDir() = %q, want %q", got, want)
		}
	})

	t.Run("XDG_CACHE_HOME/huggingface/hub when HF_HOME is unset", func(t *testing.T) {
		t.Setenv("HF_HUB_CACHE", "")
		t.Setenv("HUGGINGFACE_HUB_CACHE", "")
		t.Setenv("HF_HOME", "")
		t.Setenv("XDG_CACHE_HOME", "/custom/xdg-cache")
		want := filepath.Join("/custom/xdg-cache", "huggingface", "hub")
		if got := hfCacheBaseDir(); got != want {
			t.Errorf("hfCacheBaseDir() = %q, want %q", got, want)
		}
	})
}

// TestRunDiskPreflightInjected exercises the glue function with the
// free-space and size-estimate funcs both injected (per the issue's testing
// guidance), so it needs no real disk or network access.
func TestRunDiskPreflightInjected(t *testing.T) {
	origTree, origDisk, origCacheSize := hfRepoTreeFn, availableDiskBytesFn, hfCacheModelSizeFn
	t.Cleanup(func() { hfRepoTreeFn, availableDiskBytesFn, hfCacheModelSizeFn = origTree, origDisk, origCacheSize })
	// Every sub-test below that doesn't care about caching must see "nothing
	// cached", matching pre-fix behavior -- only the caching-specific
	// sub-tests override this.
	hfCacheModelSizeFn = func(modelName string) int64 { return 0 }

	jc := JobContext{}

	t.Run("blocks and downloads nothing when the estimate exceeds free space", func(t *testing.T) {
		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return []hfTreeEntry{
				{Type: "file", Path: "huge.safetensors", Size: 161 << 30},
			}, nil
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			return 50 << 30, nil // only 50GiB free
		}

		allow, ignore, err := runDiskPreflight(jc, "Lightricks/LTX-Video", nil, nil, diskSafetyMarginBytes)
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
				{Type: "file", Path: "model.safetensors", Size: 5 << 30},
			}, nil
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			return 500 << 30, nil
		}

		_, _, err := runDiskPreflight(jc, "some/small-model", nil, nil, diskSafetyMarginBytes)
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

		allow, ignore, err := runDiskPreflight(jc, "some/model", []string{"a/*"}, []string{"b/*"}, diskSafetyMarginBytes)
		if err != nil {
			t.Fatalf("expected fail-open (nil error) on metadata fetch failure, got %v", err)
		}
		// Caller's original patterns must survive unchanged.
		if !equalStrs(allow, []string{"a/*"}) || !equalStrs(ignore, []string{"b/*"}) {
			t.Errorf("expected caller patterns to pass through unchanged, got allow=%v ignore=%v", allow, ignore)
		}
	})

	t.Run("fails open (proceeds, no error) when the disk-free read errors", func(t *testing.T) {
		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return []hfTreeEntry{{Type: "file", Path: "model.safetensors", Size: 5 << 30}}, nil
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			return 0, errors.New("statfs not supported")
		}

		_, _, err := runDiskPreflight(jc, "some/model", nil, nil, diskSafetyMarginBytes)
		if err != nil {
			t.Fatalf("expected fail-open (nil error) on disk-free read failure, got %v", err)
		}
	})

	t.Run("proceeds when the tree fetch succeeds but reports no usable sizes", func(t *testing.T) {
		// Distinct from the "metadata fetch errors" case above: here the HF tree
		// API call itself succeeds, but every entry's size is unknown/zero (an
		// older API shape, or a repo the tree endpoint can list but not size).
		// sumFilteredSize then totals 0, which must NOT be misread as "an empty
		// download fits" turning into a false refusal, nor read the disk at all
		// -- it degrades to "nothing to gate on" and proceeds, same direction as
		// the fetch-error fail-open path.
		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return []hfTreeEntry{
				{Type: "file", Path: "model.safetensors", Size: 0},
				{Type: "file", Path: "config.json", Size: 0},
			}, nil
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			t.Error("availableDiskBytesFn must not be called when requiredBytes is 0 (nothing to gate on)")
			return 0, nil
		}

		allow, ignore, err := runDiskPreflight(jc, "some/unsized-model", nil, nil, diskSafetyMarginBytes)
		if err != nil {
			t.Fatalf("expected no error when the size estimate is unknown/zero, got %v", err)
		}
		if allow != nil || ignore != nil {
			t.Errorf("expected caller's (empty) patterns to pass through unchanged, got allow=%v ignore=%v", allow, ignore)
		}
	})

	t.Run("auto-selects diffusers subfolders when caller supplied no patterns (#828 part 3)", func(t *testing.T) {
		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return []hfTreeEntry{
				{Type: "file", Path: "ltx-video-a.safetensors", Size: 80 << 30},
				{Type: "file", Path: "ltx-video-b.safetensors", Size: 80 << 30},
				{Type: "file", Path: "transformer/model.safetensors", Size: 10 << 30},
				{Type: "file", Path: "vae/model.safetensors", Size: 2 << 30},
				{Type: "file", Path: "text_encoder/model.safetensors", Size: 5 << 30},
				{Type: "file", Path: "tokenizer/vocab.json", Size: 1 << 20},
				{Type: "file", Path: "scheduler/scheduler_config.json", Size: 1 << 10},
			}, nil
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			return 100 << 30, nil // enough for the filtered ~17GB, not the full ~160GB
		}

		allow, _, err := runDiskPreflight(jc, "Lightricks/LTX-Video", nil, nil, diskSafetyMarginBytes)
		if err != nil {
			t.Fatalf("expected the diffusers-filtered pull to fit and proceed, got error: %v", err)
		}
		if allow == nil {
			t.Fatal("expected auto-derived allow patterns, got nil")
		}
	})

	// citadel#840 review (BLOCKING regression): MODEL_CACHE_PULL redeploys on
	// every deploy, even a model already sitting in the local HF hub cache.
	// `hf download` (no --local-dir) is resumable/idempotent -- already-present
	// blobs are not re-fetched -- so requiredBytes must be netted against what's
	// already cached, or a redeploy of an already-cached model on a
	// now-tight-on-disk node (tight BECAUSE that cache is what filled it) fails
	// closed on a pull that would actually download nothing.
	t.Run("proceeds when the model is already fully cached, even though the full repo size would not fit", func(t *testing.T) {
		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return []hfTreeEntry{
				{Type: "file", Path: "huge.safetensors", Size: 161 << 30},
			}, nil
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			// Nowhere close to covering the full 161GB -- if the fix regressed,
			// this alone would fail the preflight closed.
			return 5 << 30, nil
		}
		origCache := hfCacheModelSizeFn
		hfCacheModelSizeFn = func(modelName string) int64 { return 161 << 30 } // fully cached
		t.Cleanup(func() { hfCacheModelSizeFn = origCache })

		_, _, err := runDiskPreflight(jc, "Lightricks/LTX-Video", nil, nil, diskSafetyMarginBytes)
		if err != nil {
			t.Fatalf("expected a fully-cached model to proceed as a no-op redeploy, got blocking error: %v", err)
		}
	})

	t.Run("partially cached: gates on the REMAINING bytes, not the full repo size", func(t *testing.T) {
		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return []hfTreeEntry{
				{Type: "file", Path: "huge.safetensors", Size: 161 << 30},
			}, nil
		}
		// Only 20GiB free -- would fail against the full 161GB, but 150GB is
		// already cached, so only ~11GB (well under the margin-adjusted
		// available) actually needs to download.
		availableDiskBytesFn = func(path string) (uint64, error) {
			return 20 << 30, nil
		}
		origCache := hfCacheModelSizeFn
		hfCacheModelSizeFn = func(modelName string) int64 { return 150 << 30 }
		t.Cleanup(func() { hfCacheModelSizeFn = origCache })

		_, _, err := runDiskPreflight(jc, "Lightricks/LTX-Video", nil, nil, diskSafetyMarginBytes)
		if err != nil {
			t.Fatalf("expected the ~11GB REMAINING download to fit in 20GiB free, got blocking error: %v", err)
		}
	})

	t.Run("partially cached but the remaining bytes still don't fit: still fails closed", func(t *testing.T) {
		// Proves netting is not a blanket "any caching at all skips the gate"
		// shortcut -- a partial cache that leaves a genuinely-too-large
		// remainder must still block.
		hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
			return []hfTreeEntry{
				{Type: "file", Path: "huge.safetensors", Size: 161 << 30},
			}, nil
		}
		availableDiskBytesFn = func(path string) (uint64, error) {
			return 20 << 30, nil // 20GiB free
		}
		origCache := hfCacheModelSizeFn
		// Only 50GB cached -- 111GB would still need to download, which does not
		// fit in 20GiB free.
		hfCacheModelSizeFn = func(modelName string) int64 { return 50 << 30 }
		t.Cleanup(func() { hfCacheModelSizeFn = origCache })

		err := func() error {
			_, _, err := runDiskPreflight(jc, "Lightricks/LTX-Video", nil, nil, diskSafetyMarginBytes)
			return err
		}()
		if err == nil {
			t.Fatal("expected a blocking error: the remaining ~111GB does not fit in 20GiB free even after netting the 50GB cached")
		}
		if !strings.Contains(err.Error(), "insufficient disk space") {
			t.Errorf("error = %v, want an 'insufficient disk space' message", err)
		}
	})
}
