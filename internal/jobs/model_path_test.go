package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unsafeModelComponents are refused as a single path component on every OS.
var unsafeModelComponents = []string{
	"",
	".",
	"..",
	"...",
	"../evil",
	"../../etc/passwd",
	"a/../../evil",
	"sub/file.gguf",
	"/etc/passwd",
	"/abs",
	`..\evil`,
	`..\..\Windows\System32`,
	`sub\file.gguf`,
	`C:\Windows\evil.gguf`,
	`C:evil.gguf`,
	"C:",
	`\\server\share\evil.gguf`,
	`\\?\C:\evil.gguf`,
	"evil\x00.gguf",
	"name.",
	"name ",
	" name",
	"line\nbreak",
	"NUL",
	"con.txt",
	"COM1.gguf",
	"lpt9",
	"file:stream",
}

// withDownloadModelCacheRoot points DOWNLOAD_MODEL at a temp cache root for
// the duration of the test.
func withDownloadModelCacheRoot(t *testing.T, root string) {
	t.Helper()
	orig := downloadModelCacheRoot
	downloadModelCacheRoot = func() string { return root }
	t.Cleanup(func() { downloadModelCacheRoot = orig })
}

func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected %s to stay empty, found %d entries", dir, len(entries))
	}
}

func assertUnsafeRefusal(t *testing.T, err error, input string) {
	t.Helper()
	if !errors.Is(err, ErrUnsafeModelPath) {
		t.Fatalf("expected ErrUnsafeModelPath, got %v", err)
	}
	// Short inputs can coincide with words in the fixed message.
	if len(input) >= 6 && strings.Contains(err.Error(), input) {
		t.Fatalf("error echoes the payload value: %v", err)
	}
}

func TestValidModelPathComponent(t *testing.T) {
	for _, s := range unsafeModelComponents {
		if validModelPathComponent(s) {
			t.Errorf("validModelPathComponent(%q) = true, want false", s)
		}
	}
	for _, s := range []string{
		"llamacpp",
		"llama-2-7b-chat.Q4_K_M.gguf",
		"model_v1.0.safetensors",
		"..hidden-but-local",
		"console.gguf",
		"com10",
	} {
		if !validModelPathComponent(s) {
			t.Errorf("validModelPathComponent(%q) = false, want true", s)
		}
	}
}

func TestValidHFRepoID(t *testing.T) {
	for _, id := range []string{"gpt2", "org/model", "stabilityai/stable-diffusion-3.5-medium", "TheBloke/Llama-2-7B-Chat-GGUF"} {
		if !validHFRepoID(id) {
			t.Errorf("validHFRepoID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{
		"", "a/b/c", "../x", "org/..", "/org/model", "org/", "/model",
		`org\..\..\x`, `C:\x`, "C:x", `\\srv\share`, "org/model\x00",
		"--local-dir=/tmp/x", "-x/model", ".hidden/model",
	} {
		if validHFRepoID(id) {
			t.Errorf("validHFRepoID(%q) = true, want false", id)
		}
	}
}

func TestDownloadModel_RefusesUnsafePathFields(t *testing.T) {
	for _, field := range []string{"model_type", "file_name"} {
		for _, bad := range unsafeModelComponents {
			t.Run(field+"/"+bad, func(t *testing.T) {
				base := t.TempDir()
				root := filepath.Join(base, "cache")
				outside := filepath.Join(base, "outside")
				if err := os.MkdirAll(outside, 0o755); err != nil {
					t.Fatal(err)
				}
				withDownloadModelCacheRoot(t, root)

				payload := map[string]string{
					"repo_url":   "https://huggingface.co/org/repo",
					"model_type": "llamacpp",
					"file_name":  "model.gguf",
				}
				payload[field] = bad
				h := &DownloadModelHandler{}
				_, err := h.Execute(JobContext{}, makeJob(payload))
				assertUnsafeRefusal(t, err, bad)
				assertDirEmpty(t, outside)
				if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("cache root should not be created on refusal (stat err %v)", statErr)
				}
			})
		}
	}
}

func TestDownloadModel_RefusesNonHTTPRepoURL(t *testing.T) {
	withDownloadModelCacheRoot(t, filepath.Join(t.TempDir(), "cache"))
	for _, u := range []string{"", "file:///etc/passwd", "-K/tmp/cfg", "ftp://host/x", "https://", "huggingface.co/org/repo"} {
		h := &DownloadModelHandler{}
		_, err := h.Execute(JobContext{}, makeJob(map[string]string{
			"repo_url":   u,
			"model_type": "llamacpp",
			"file_name":  "model.gguf",
		}))
		if !errors.Is(err, errInvalidModelRepoURL) {
			t.Errorf("repo_url %q: expected errInvalidModelRepoURL, got %v", u, err)
		}
	}
}

// TestDownloadModel_ValidExistingFile exercises the normal path without the
// network: the destination already exists, so the handler resolves it and
// returns before running curl.
func TestDownloadModel_ValidExistingFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	withDownloadModelCacheRoot(t, root)
	want := filepath.Join(root, "llamacpp", "llama-2-7b-chat.Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := &DownloadModelHandler{}
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"repo_url":   "https://huggingface.co/TheBloke/Llama-2-7B-Chat-GGUF",
		"model_type": "llamacpp",
		"file_name":  "llama-2-7b-chat.Q4_K_M.gguf",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), "already exists") || !strings.Contains(string(out), want) {
		t.Fatalf("unexpected output: %s", out)
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func TestResolveModelCacheDest_SymlinkedSubdirEscapeRefused(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "cache")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	symlinkOrSkip(t, outside, filepath.Join(root, "llamacpp"))

	_, _, _, err := resolveModelCacheDest(root, "llamacpp", "model.gguf")
	assertUnsafeRefusal(t, err, "")
	assertDirEmpty(t, outside)

	// The handler refuses the same layout end to end.
	withDownloadModelCacheRoot(t, root)
	h := &DownloadModelHandler{}
	_, err = h.Execute(JobContext{}, makeJob(map[string]string{
		"repo_url":   "https://huggingface.co/org/repo",
		"model_type": "llamacpp",
		"file_name":  "model.gguf",
	}))
	assertUnsafeRefusal(t, err, "")
	assertDirEmpty(t, outside)
}

func TestResolveModelCacheDest_SymlinkedLeafRefused(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "cache")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "llamacpp"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Dangling: a write through it would create a file outside the cache.
	symlinkOrSkip(t, filepath.Join(outside, "target.gguf"), filepath.Join(root, "llamacpp", "dangling.gguf"))
	_, _, _, err := resolveModelCacheDest(root, "llamacpp", "dangling.gguf")
	assertUnsafeRefusal(t, err, "")

	// Pointing at an existing outside file.
	existing := filepath.Join(base, "existing.gguf")
	if err := os.WriteFile(existing, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, existing, filepath.Join(root, "llamacpp", "existing.gguf"))
	_, _, _, err = resolveModelCacheDest(root, "llamacpp", "existing.gguf")
	assertUnsafeRefusal(t, err, "")
	assertDirEmpty(t, outside)
}

func TestResolveModelCacheDest_LeafDirectoryRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "llamacpp", "model.gguf"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := resolveModelCacheDest(root, "llamacpp", "model.gguf")
	assertUnsafeRefusal(t, err, "")
}

// A cache root that is itself a symlink (the whole cache on another disk) is
// allowed, and new files land inside its target.
func TestResolveModelCacheDest_SymlinkedRootAllowed(t *testing.T) {
	base := t.TempDir()
	disk := filepath.Join(base, "bigdisk")
	if err := os.MkdirAll(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "cache")
	symlinkOrSkip(t, disk, root)

	destDir, destPath, exists, err := resolveModelCacheDest(root, "llamacpp", "model.gguf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Fatal("exists = true for a new file")
	}
	if destPath != filepath.Join(destDir, "model.gguf") {
		t.Fatalf("destPath %q not under destDir %q", destPath, destDir)
	}
	if fi, err := os.Stat(filepath.Join(disk, "llamacpp")); err != nil || !fi.IsDir() {
		t.Fatalf("subdir not created inside the symlinked root's target: %v", err)
	}
}

// setHFHomeOnly points the HF hub cache at dir/hub, clearing the env vars that
// would take precedence over HF_HOME.
func setHFHomeOnly(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HF_HUB_CACHE", "")
	t.Setenv("HUGGINGFACE_HUB_CACHE", "")
	t.Setenv("HF_HOME", dir)
}

func TestHFCacheDir_RefusesUnsafeModelNames(t *testing.T) {
	hfHome := t.TempDir()
	setHFHomeOnly(t, hfHome)
	hub := filepath.Join(hfHome, "hub")
	if err := os.MkdirAll(filepath.Join(hub, "models--org--model"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := hfCacheDir("org/model"); got != filepath.Join(hub, "models--org--model") {
		t.Fatalf("hfCacheDir(valid) = %q", got)
	}
	// A directory a lexical escape would land on must never be returned.
	if err := os.MkdirAll(filepath.Join(hfHome, "victim"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		`a\..\..\victim`, `org/..`, "C:x", `\\srv\share`, "a\x00b", "org/model:stream",
	} {
		if got := hfCacheDir(name); got != "" {
			t.Errorf("hfCacheDir(%q) = %q, want empty", name, got)
		}
	}
}

func TestModelCachePull_RefusesUnsafeHFModelName(t *testing.T) {
	setHFHomeOnly(t, t.TempDir())
	for _, engine := range []string{"vllm", "llamacpp"} {
		for _, name := range []string{"../../x", `org\..\..\x`, "C:x", "a/b/c", "--local-dir=/tmp/x", "org/.."} {
			h := &ModelCachePullHandler{}
			_, err := h.Execute(JobContext{}, makeJob(map[string]string{
				"model_name": name,
				"engine":     engine,
			}))
			assertUnsafeRefusal(t, err, name)
		}
	}
}

func TestModelCacheEvict_RefusesUnsafeHFModelName(t *testing.T) {
	hfHome := t.TempDir()
	setHFHomeOnly(t, hfHome)
	victim := filepath.Join(hfHome, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../victim", `a\..\..\victim`, "C:x", "org/.."} {
		h := &ModelCacheEvictHandler{}
		_, err := h.Execute(JobContext{}, makeJob(map[string]string{
			"model_name": name,
			"engine":     "vllm",
		}))
		assertUnsafeRefusal(t, err, name)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim dir removed: %v", err)
	}
}
