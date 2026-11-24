package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCachedAllowedLogins(t *testing.T) {
	// Disable the racy-window guard so cache hits are observable on fresh files.
	old := allowlistRacyWindow
	allowlistRacyWindow = 0
	t.Cleanup(func() { allowlistRacyWindow = old })
	path := filepath.Join(t.TempDir(), "platform-exposure.json")
	loads := 0
	load := func() (map[string]bool, error) {
		loads++
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		if err != nil {
			return nil, err
		}
		if string(data) == "bad" {
			return nil, errors.New("malformed")
		}
		return map[string]bool{string(data): true}, nil
	}
	get := cachedAllowedLogins(path, load)

	// Absent file: empty allowlist, cached.
	for i := 0; i < 3; i++ {
		m, err := get()
		if err != nil || len(m) != 0 {
			t.Fatalf("absent: %v %v", m, err)
		}
	}
	if loads != 1 {
		t.Fatalf("absent file loaded %d times, want 1", loads)
	}

	// File appears: picked up.
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _ := get()
	if !m["a"] || loads != 2 {
		t.Fatalf("new file not picked up: %v loads=%d", m, loads)
	}
	_, _ = get()
	if loads != 2 {
		t.Fatalf("unchanged file reloaded: loads=%d", loads)
	}

	// Same size, new mtime: picked up.
	if err := os.WriteFile(path, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	m, _ = get()
	if !m["b"] || m["a"] {
		t.Fatalf("changed file not picked up: %v", m)
	}

	// Malformed file: error, not cached, keeps failing closed; fixing it recovers.
	if err := os.WriteFile(path, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := get(); err == nil {
			t.Fatal("malformed file must return an error")
		}
	}
	if err := os.WriteFile(path, []byte("ok-now"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := get()
	if err != nil || !m["ok-now"] {
		t.Fatalf("recovery failed: %v %v", m, err)
	}
}

func newFileBackedCache(t *testing.T) (string, func() (map[string]bool, error)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "platform-exposure.json")
	load := func() (map[string]bool, error) {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		if err != nil {
			return nil, err
		}
		return map[string]bool{string(data): true}, nil
	}
	return path, cachedAllowedLogins(path, load)
}

func TestCachedAllowedLogins_SameSizeRewriteWithMtimeRestored(t *testing.T) {
	path, get := newFileBackedCache(t)
	if err := os.WriteFile(path, []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	orig := fi.ModTime()
	if m, _ := get(); !m["aaaa"] {
		t.Fatal("initial load failed")
	}
	if err := os.WriteFile(path, []byte("bbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, orig, orig); err != nil {
		t.Fatal(err)
	}
	m, err := get()
	if err != nil || !m["bbbb"] || m["aaaa"] {
		t.Fatalf("same-size, mtime-restored rewrite not picked up: %v %v", m, err)
	}
}

func TestCachedAllowedLogins_ChmodFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	path, get := newFileBackedCache(t)
	if err := os.WriteFile(path, []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := get(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if m, err := get(); err == nil {
		t.Fatalf("unreadable file must surface the loader error, got %v", m)
	}
}

func TestCachedAllowedLogins_RenameReplace(t *testing.T) {
	path, get := newFileBackedCache(t)
	if err := os.WriteFile(path, []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	orig := fi.ModTime()
	if _, err := get(); err != nil {
		t.Fatal(err)
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte("cccc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, orig, orig); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	m, err := get()
	if err != nil || !m["cccc"] {
		t.Fatalf("rename-replace not picked up: %v %v", m, err)
	}
}

func TestCachedAllowedLogins_FreshFileIsNotCached(t *testing.T) {
	loads := 0
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	get := cachedAllowedLogins(path, func() (map[string]bool, error) { loads++; return map[string]bool{}, nil })
	_, _ = get()
	_, _ = get()
	if loads != 2 {
		t.Fatalf("a just-written file must be re-read, loads=%d", loads)
	}
}
