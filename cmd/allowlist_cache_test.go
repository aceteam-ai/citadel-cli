package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCachedAllowedLogins(t *testing.T) {
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
