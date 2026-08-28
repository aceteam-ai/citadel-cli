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

// clearHFEnv unsets every env var hfCacheBaseDir/hfDownloadEnv consult, so a
// test starts from "operator configured nothing" regardless of what the host
// running `go test` happens to have set.
func clearHFEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HF_HUB_CACHE", "HUGGINGFACE_HUB_CACHE", "HF_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(k, "")
	}
}

// hfHomeFromEnv extracts the value of HF_HOME from an env slice (as returned
// by hfDownloadEnv), or "" if absent. Mirrors how the OS resolves env for a
// subprocess: the LAST occurrence of a key wins for exec's envp (this
// package never emits duplicates, but scanning to the end keeps the helper
// correct even if that ever changes).
func hfHomeFromEnv(env []string) string {
	val := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "HF_HOME=") {
			val = strings.TrimPrefix(kv, "HF_HOME=")
		}
	}
	return val
}

// TestHFCachePathsAgree pins the citadel #682 P0 fix and its #840
// coordination requirement in one place: the directory pullHuggingFace's
// subprocess actually downloads into (HF_HOME on hfDownloadEnv()), the
// canonical citadel cache directory the engine containers mount
// (canonicalHFCacheDir()), and the directory the disk preflight/no-op
// detection reads free space and "already cached" size from
// (hfCacheBaseDir()) must all agree. Hermetic: no real `hf` binary or
// network access, just the env-construction seam.
func TestHFCachePathsAgree(t *testing.T) {
	t.Run("no operator override: subprocess env, canonical dir, and hfCacheBaseDir all agree", func(t *testing.T) {
		clearHFEnv(t)

		wantCanonical := canonicalHFCacheDir()
		if wantCanonical == "" {
			t.Fatal("canonicalHFCacheDir() returned empty")
		}

		// What the download subprocess will actually write into.
		gotHFHome := hfHomeFromEnv(hfDownloadEnv())
		if gotHFHome != wantCanonical {
			t.Errorf("hfDownloadEnv() sets HF_HOME=%q, want %q (canonicalHFCacheDir())", gotHFHome, wantCanonical)
		}

		// What the disk preflight / no-op detection / MODEL_CACHE_EVICT believe
		// the hub cache directory is (HF_HOME/hub, since no HF_HUB_CACHE/
		// HUGGINGFACE_HUB_CACHE is set).
		wantHubDir := filepath.Join(wantCanonical, "hub")
		if got := hfCacheBaseDir(); got != wantHubDir {
			t.Errorf("hfCacheBaseDir() = %q, want %q (canonicalHFCacheDir()+\"/hub\")", got, wantHubDir)
		}

		// The subprocess's own hub dir (what `hf download` with HF_HOME=gotHFHome
		// actually resolves to) must be the SAME directory hfCacheBaseDir() reads.
		subprocessHubDir := filepath.Join(gotHFHome, "hub")
		if subprocessHubDir != wantHubDir {
			t.Errorf("subprocess would write under %q, but hfCacheBaseDir() reads %q -- divergence reintroduced", subprocessHubDir, wantHubDir)
		}
	})

	t.Run("operator HF_HUB_CACHE override: subprocess env respects it, does not inject HF_HOME", func(t *testing.T) {
		clearHFEnv(t)
		t.Setenv("HF_HUB_CACHE", "/mnt/models/hub")

		if got := hfHomeFromEnv(hfDownloadEnv()); got != "" {
			t.Errorf("hfDownloadEnv() injected HF_HOME=%q despite an explicit HF_HUB_CACHE override", got)
		}
		if got := hfCacheBaseDir(); got != "/mnt/models/hub" {
			t.Errorf("hfCacheBaseDir() = %q, want /mnt/models/hub (the operator override)", got)
		}
	})

	t.Run("operator HF_HOME override: subprocess env respects it, does not inject a second HF_HOME", func(t *testing.T) {
		clearHFEnv(t)
		t.Setenv("HF_HOME", "/custom/home")

		if got := hfHomeFromEnv(hfDownloadEnv()); got != "/custom/home" {
			t.Errorf("hfDownloadEnv() HF_HOME = %q, want /custom/home (the operator's own value, untouched)", got)
		}
		want := filepath.Join("/custom/home", "hub")
		if got := hfCacheBaseDir(); got != want {
			t.Errorf("hfCacheBaseDir() = %q, want %q", got, want)
		}
	})

	t.Run("hfCacheDir resolves under the same base hfCacheBaseDir reports", func(t *testing.T) {
		clearHFEnv(t)
		// hfCacheDir stats the resolved path and returns "" when absent -- we
		// only need the base-directory computation to agree, which we can check
		// indirectly: a model whose cache dir exists under hfCacheBaseDir()
		// would be found there and nowhere else. Assert the (nonexistent) path
		// it would have looked for is under hfCacheBaseDir(), not the old
		// ~/.cache/huggingface default.
		base := hfCacheBaseDir()
		if !strings.Contains(base, filepath.Join("citadel-cache", "huggingface")) {
			t.Errorf("hfCacheBaseDir() = %q, does not resolve under citadel-cache/huggingface (the canonical, container-mounted path)", base)
		}
	})

	// The three sub-tests above pin hfDownloadEnv()/hfCacheBaseDir() agreeing
	// as ingredients -- they do NOT prove pullHuggingFace's real download
	// actually applies that env. buildHFPullCommand is the one call site that
	// does (pullHuggingFace calls exactly this, nothing else), so assert on
	// ITS output, not just the helper it's built from.
	t.Run("buildHFPullCommand (pullHuggingFace's actual command) carries the fix", func(t *testing.T) {
		clearHFEnv(t)
		cmd := buildHFPullCommand("hf", "meta-llama/Llama-2-7b-chat-hf", nil, nil)
		if got := hfHomeFromEnv(cmd.Env); got != canonicalHFCacheDir() {
			t.Errorf("buildHFPullCommand(...).Env has HF_HOME=%q, want %q -- pullHuggingFace's actual subprocess would not write to the canonical cache", got, canonicalHFCacheDir())
		}
		// argv is unaffected -- byte-identical to the pure-argv builder.
		wantArgv := BuildHuggingFaceDownloadCommandFiltered("hf", "meta-llama/Llama-2-7b-chat-hf", nil, nil).Args
		if !equalStrs(cmd.Args, wantArgv) {
			t.Errorf("buildHFPullCommand(...).Args = %v, want %v", cmd.Args, wantArgv)
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
