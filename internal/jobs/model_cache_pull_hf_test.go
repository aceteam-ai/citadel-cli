package jobs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestHFDownloadArgs pins the shared `download <repo> [file] [--local-dir]`
// grammar used by both `hf` and the deprecated `huggingface-cli` (citadel #566).
func TestHFDownloadArgs(t *testing.T) {
	// Single-file pull (bonsai path): repo, file, and --local-dir all present,
	// with the file immediately following the repo id.
	got := hfDownloadArgs("prism-ml/Bonsai-27B-gguf", "Bonsai-27B-Q1_0.gguf", "/cache/bonsai")
	want := []string{"download", "prism-ml/Bonsai-27B-gguf", "Bonsai-27B-Q1_0.gguf", "--local-dir", "/cache/bonsai"}
	if !equalStrs(got, want) {
		t.Errorf("single-file args = %v, want %v", got, want)
	}

	// Repo pull (vllm/llamacpp path): no file, no --local-dir (lands in hub cache).
	got = hfDownloadArgs("meta-llama/Llama-2-7b-chat-hf", "", "")
	want = []string{"download", "meta-llama/Llama-2-7b-chat-hf"}
	if !equalStrs(got, want) {
		t.Errorf("repo args = %v, want %v", got, want)
	}
}

// TestResolveHFDownloaderPrefersHF verifies that when both binaries are present,
// the modern `hf` wins over the deprecated (no-op) `huggingface-cli`.
func TestResolveHFDownloaderPrefersHF(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-executable PATH resolution is unix-specific")
	}
	dir := t.TempDir()
	writeFakeExe(t, filepath.Join(dir, "hf"))
	writeFakeExe(t, filepath.Join(dir, "huggingface-cli"))
	t.Setenv("PATH", dir)
	t.Setenv("HOME", t.TempDir()) // neutralize hfBinDirs fallbacks

	bin, err := resolveHFDownloader()
	if err != nil {
		t.Fatalf("resolveHFDownloader: %v", err)
	}
	if filepath.Base(bin) != "hf" {
		t.Errorf("resolved %q, want the modern `hf` binary to be preferred", bin)
	}
}

// TestResolveHFDownloaderFallsBackToLegacy verifies that when only the legacy
// binary is present, it is used (older envs without `hf`).
func TestResolveHFDownloaderFallsBackToLegacy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-executable PATH resolution is unix-specific")
	}
	dir := t.TempDir()
	writeFakeExe(t, filepath.Join(dir, "huggingface-cli"))
	t.Setenv("PATH", dir)
	t.Setenv("HOME", t.TempDir())

	bin, err := resolveHFDownloader()
	if err != nil {
		t.Fatalf("resolveHFDownloader: %v", err)
	}
	if filepath.Base(bin) != "huggingface-cli" {
		t.Errorf("resolved %q, want huggingface-cli fallback", bin)
	}
}

// TestVerifyDownloadedFile covers the no-op detection signal: an absent or empty
// output file must be an error (the huggingface-cli no-op leaves --local-dir
// empty), while a non-empty file returns its size.
func TestVerifyDownloadedFile(t *testing.T) {
	dir := t.TempDir()

	// Missing file -> error.
	if _, err := verifyDownloadedFile(filepath.Join(dir, "missing.gguf")); err == nil {
		t.Error("expected error for missing file, got nil")
	}

	// Empty file (the no-op signature) -> error.
	empty := filepath.Join(dir, "empty.gguf")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyDownloadedFile(empty); err == nil {
		t.Error("expected error for empty file, got nil")
	}

	// Non-empty file -> size returned.
	full := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(full, []byte("GGUFdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	size, err := verifyDownloadedFile(full)
	if err != nil {
		t.Fatalf("unexpected error for non-empty file: %v", err)
	}
	if size != 8 {
		t.Errorf("size = %d, want 8", size)
	}

	// A directory at the path -> error (not a file).
	if _, err := verifyDownloadedFile(dir); err == nil {
		t.Error("expected error when path is a directory, got nil")
	}
}

func writeFakeExe(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
