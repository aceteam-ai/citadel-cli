package jobs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/cacheindex"
	"github.com/aceteam-ai/citadel-cli/services"
)

// fakeModelDownload replaces the curl runner for the test. It records each
// call's args and writes body to the destination, then returns err.
type fakeModelDownload struct {
	calls [][]string
	body  string
	err   error
}

func installFakeModelDownload(t *testing.T, f *fakeModelDownload) {
	t.Helper()
	orig := runModelDownload
	runModelDownload = func(_ context.Context, args []string, out io.Writer) ([]byte, error) {
		f.calls = append(f.calls, append([]string(nil), args...))
		if _, err := io.WriteString(out, f.body); err != nil {
			return nil, err
		}
		return nil, f.err
	}
	t.Cleanup(func() { runModelDownload = orig })
}

func installBeforeOpen(t *testing.T, fn func()) {
	t.Helper()
	orig := downloadModelBeforeOpen
	downloadModelBeforeOpen = fn
	t.Cleanup(func() { downloadModelBeforeOpen = orig })
}

func validDownloadPayload() map[string]string {
	return map[string]string{
		"repo_url":   "https://huggingface.co/org/repo",
		"model_type": "llamacpp",
		"file_name":  "model.gguf",
	}
}

func TestModelDownloadCurlArgs(t *testing.T) {
	got := modelDownloadCurlArgs("https://huggingface.co/org/repo/resolve/main/model.gguf")
	want := []string{
		"--fail", "--silent", "--show-error", "--location",
		"--proto", "=https,http", "--proto-redir", "=https,http",
		"--", "https://huggingface.co/org/repo/resolve/main/model.gguf",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("curl args = %v, want %v", got, want)
	}
}

func TestDownloadModel_WritesThroughCreatedFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	withDownloadModelCacheRoot(t, root)
	fake := &fakeModelDownload{body: "gguf-bytes"}
	installFakeModelDownload(t, fake)

	h := &DownloadModelHandler{}
	if _, err := h.Execute(JobContext{}, makeJob(validDownloadPayload())); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("download ran %d times, want 1", len(fake.calls))
	}
	want := modelDownloadCurlArgs("https://huggingface.co/org/repo/resolve/main/model.gguf")
	if !reflect.DeepEqual(fake.calls[0], want) {
		t.Fatalf("curl args = %v, want %v", fake.calls[0], want)
	}
	data, err := os.ReadFile(filepath.Join(root, "llamacpp", "model.gguf"))
	if err != nil || string(data) != "gguf-bytes" {
		t.Fatalf("downloaded file = %q, %v", data, err)
	}
}

func TestDownloadModel_FailedDownloadLeavesNoFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	withDownloadModelCacheRoot(t, root)
	installFakeModelDownload(t, &fakeModelDownload{body: "partial", err: errors.New("exit status 22")})

	h := &DownloadModelHandler{}
	if _, err := h.Execute(JobContext{}, makeJob(validDownloadPayload())); err == nil {
		t.Fatal("expected an error from a failed download")
	}
	if _, err := os.Lstat(filepath.Join(root, "llamacpp", "model.gguf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file left behind (lstat err %v)", err)
	}
}

// A symlink planted at the leaf after validation is never followed.
func TestDownloadModel_LeafSymlinkPlantedAfterValidation(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "cache")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	withDownloadModelCacheRoot(t, root)
	fake := &fakeModelDownload{body: "x"}
	installFakeModelDownload(t, fake)
	installBeforeOpen(t, func() {
		symlinkOrSkip(t, filepath.Join(outside, "target.gguf"), filepath.Join(root, "llamacpp", "model.gguf"))
	})

	h := &DownloadModelHandler{}
	_, err := h.Execute(JobContext{}, makeJob(validDownloadPayload()))
	assertUnsafeRefusal(t, err, "")
	if len(fake.calls) != 0 {
		t.Fatal("download ran after the leaf changed")
	}
	assertDirEmpty(t, outside)
}

// A subdirectory swapped for an outside symlink after validation is never
// written through.
func TestDownloadModel_SubdirSwappedAfterValidation(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "cache")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	withDownloadModelCacheRoot(t, root)
	fake := &fakeModelDownload{body: "x"}
	installFakeModelDownload(t, fake)
	installBeforeOpen(t, func() {
		sub := filepath.Join(root, "llamacpp")
		if err := os.Remove(sub); err != nil {
			t.Fatal(err)
		}
		symlinkOrSkip(t, outside, sub)
	})

	h := &DownloadModelHandler{}
	if _, err := h.Execute(JobContext{}, makeJob(validDownloadPayload())); err == nil {
		t.Fatal("expected a refusal after the subdirectory was swapped")
	}
	if len(fake.calls) != 0 {
		t.Fatal("download ran after the subdirectory changed")
	}
	assertDirEmpty(t, outside)
}

func TestModelCachePull_RefusesOptionLikePatterns(t *testing.T) {
	setHFHomeOnly(t, t.TempDir())
	for _, engine := range []string{"vllm", "llamacpp"} {
		for _, field := range []string{"allow_patterns", "ignore_patterns"} {
			for _, raw := range []string{`["--local-dir=/tmp/x"]`, `["ok/*","-x"]`, "ok/*,--token=abc"} {
				h := &ModelCachePullHandler{}
				_, err := h.Execute(JobContext{}, makeJob(map[string]string{
					"model_name": "org/model",
					"engine":     engine,
					field:        raw,
				}))
				if !errors.Is(err, ErrUnsafeModelArgument) {
					t.Errorf("%s %s=%s: expected ErrUnsafeModelArgument, got %v", engine, field, raw, err)
				}
			}
		}
	}
}

func TestHFDownloadArgs_PatternsAreSingleTokens(t *testing.T) {
	got := hfDownloadArgsFiltered("org/model", "", "", []string{"a/*"}, []string{"*.bin"})
	want := []string{"download", "org/model", "--include=a/*", "--exclude=*.bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestOllamaModelNames_RefuseOptionLikeValues(t *testing.T) {
	for _, name := range []string{"-h", "--insecure", "model\nname"} {
		if _, err := (&OllamaPullHandler{}).Execute(JobContext{}, makeJob(map[string]string{"model": name})); !errors.Is(err, ErrUnsafeModelArgument) {
			t.Errorf("OLLAMA_PULL %q: got %v", name, err)
		}
		if _, err := (&ModelCachePullHandler{}).Execute(JobContext{}, makeJob(map[string]string{"model_name": name, "engine": "ollama"})); !errors.Is(err, ErrUnsafeModelArgument) {
			t.Errorf("MODEL_CACHE_PULL ollama %q: got %v", name, err)
		}
		if _, err := (&ModelCacheEvictHandler{}).Execute(JobContext{}, makeJob(map[string]string{"model_name": name, "engine": "ollama"})); !errors.Is(err, ErrUnsafeModelArgument) {
			t.Errorf("MODEL_CACHE_EVICT ollama %q: got %v", name, err)
		}
		if _, err := runOllamaPull(name); !errors.Is(err, ErrUnsafeModelArgument) {
			t.Errorf("runOllamaPull %q: got %v", name, err)
		}
		if _, err := runOllamaPullViaDocker("citadel-ollama", name); !errors.Is(err, ErrUnsafeModelArgument) {
			t.Errorf("runOllamaPullViaDocker %q: got %v", name, err)
		}
	}
	for _, name := range []string{"llama3.2", "llama3.2:7b", "hf.co/org/repo:Q4_K_M"} {
		if !validModelArgument(name) {
			t.Errorf("validModelArgument(%q) = false, want true", name)
		}
	}
}

func TestModelCacheEvict_LlamaCppRefusesUnsafeName(t *testing.T) {
	dir := t.TempDir()
	orig := llamaCppCacheDirFn
	llamaCppCacheDirFn = func() string { return dir }
	t.Cleanup(func() { llamaCppCacheDirFn = orig })
	for _, name := range []string{"..", "org/..", `..\x.gguf`, "C:x.gguf", ""} {
		_, err := (&ModelCacheEvictHandler{}).Execute(JobContext{}, makeJob(map[string]string{"model_name": name, "engine": "llamacpp"}))
		if name == "" {
			if err == nil {
				t.Error("expected error for empty model_name")
			}
			continue
		}
		assertUnsafeRefusal(t, err, name)
	}
}

// Cache-index entries are local state, but a damaged or hostile Model or
// Files value must still never delete outside its cache dir.
func TestDefaultCacheGCDeleteEntry_HostileEntries(t *testing.T) {
	cacheRoot := t.TempDir()
	hfHome := t.TempDir()
	setHFHomeOnly(t, hfHome)
	victim := filepath.Join(hfHome, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	ggufDir := filepath.Join(cacheRoot, services.LlamaCppCacheDirName)
	if err := os.MkdirAll(ggufDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, model := range []string{`a\..\..\victim`, "org/..", "C:victim"} {
		if err := defaultCacheGCDeleteEntry(cacheRoot, cacheindex.Entry{
			CacheDir: services.HFHubCacheDirName,
			Family:   services.CacheFamilyHFHub,
			Model:    model,
		}); err == nil {
			t.Errorf("hf-hub model %q: expected an error", model)
		}
	}
	for _, file := range []string{"", ".", "../victim", "sub/../.."} {
		if err := defaultCacheGCDeleteEntry(cacheRoot, cacheindex.Entry{
			CacheDir: services.LlamaCppCacheDirName,
			Family:   services.CacheFamilyGGUFDir,
			Model:    "org/model",
			Files:    []string{file},
		}); err == nil {
			t.Errorf("gguf file %q: expected an error", file)
		}
	}
	if err := defaultCacheGCDeleteEntry(cacheRoot, cacheindex.Entry{
		CacheDir: services.EngineCacheDirs["ollama"].Dir,
		Family:   services.CacheFamilyNative,
		Model:    "--help",
	}); !errors.Is(err, ErrUnsafeModelArgument) {
		t.Errorf("native model starting with '-': got %v", err)
	}

	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim removed: %v", err)
	}
	if _, err := os.Stat(ggufDir); err != nil {
		t.Fatalf("gguf cache dir removed: %v", err)
	}
}
