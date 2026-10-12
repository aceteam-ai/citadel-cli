package config

import (
	"path/filepath"
	"testing"
)

func TestLoadRootsMissingReturnsEmpty(t *testing.T) {
	r := LoadRoots(t.TempDir())
	if len(r.Roots) != 0 {
		t.Fatalf("fresh config dir should have no authorized roots, got %v", r.Roots)
	}
}

func TestRootsSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := t.TempDir()
	b := t.TempDir()

	r := DefaultRoots()
	if added, err := r.Add(a); err != nil || !added {
		t.Fatalf("Add(a): added=%v err=%v", added, err)
	}
	if added, err := r.Add(b); err != nil || !added {
		t.Fatalf("Add(b): added=%v err=%v", added, err)
	}
	// Adding the same root again is a no-op.
	if added, err := r.Add(a); err != nil || added {
		t.Fatalf("re-Add(a) should be no-op: added=%v err=%v", added, err)
	}
	if err := SaveRoots(dir, r); err != nil {
		t.Fatalf("SaveRoots: %v", err)
	}

	got := LoadRoots(dir)
	if len(got.Roots) != 2 {
		t.Fatalf("expected 2 roots after round-trip, got %v", got.Roots)
	}
	// Stored roots are normalized (absolute, symlink-resolved).
	na, _ := NormalizeRoot(a)
	found := false
	for _, r := range got.Roots {
		if r == na {
			found = true
		}
		if !filepath.IsAbs(r) {
			t.Errorf("stored root %q is not absolute", r)
		}
	}
	if !found {
		t.Errorf("normalized root %q not found in %v", na, got.Roots)
	}
}

func TestRootsRemove(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	r := DefaultRoots()
	_, _ = r.Add(a)
	_, _ = r.Add(b)

	removed, err := r.Remove(a)
	if err != nil || !removed {
		t.Fatalf("Remove(a): removed=%v err=%v", removed, err)
	}
	if len(r.Roots) != 1 {
		t.Fatalf("expected 1 root after remove, got %v", r.Roots)
	}
	// Removing a non-authorized root reports false.
	removed, err = r.Remove(a)
	if err != nil || removed {
		t.Fatalf("re-Remove(a) should report not-removed: removed=%v err=%v", removed, err)
	}
}

func TestLoadRootsFromConvergence(t *testing.T) {
	primary := t.TempDir()
	legacy := t.TempDir()

	// Nothing anywhere -> empty.
	if r := LoadRootsFrom(primary, legacy); len(r.Roots) != 0 {
		t.Fatalf("no roots.yaml anywhere should be empty, got %v", r.Roots)
	}

	// Only legacy has roots.yaml -> the fallback read returns it.
	lr := DefaultRoots()
	_, _ = lr.Add(t.TempDir())
	if err := SaveRoots(legacy, lr); err != nil {
		t.Fatal(err)
	}
	if r := LoadRootsFrom(primary, legacy); len(r.Roots) != 1 {
		t.Fatalf("legacy roots should be read as a fallback, got %v", r.Roots)
	}

	// Primary now has its own roots.yaml with TWO roots -> it WINS over legacy
	// (first dir with a roots.yaml wins; no merge).
	pr := DefaultRoots()
	_, _ = pr.Add(t.TempDir())
	_, _ = pr.Add(t.TempDir())
	if err := SaveRoots(primary, pr); err != nil {
		t.Fatal(err)
	}
	if r := LoadRootsFrom(primary, legacy); len(r.Roots) != 2 {
		t.Fatalf("primary roots must win over legacy, got %v", r.Roots)
	}

	// An empty dir in the list is skipped.
	if r := LoadRootsFrom("", primary); len(r.Roots) != 2 {
		t.Fatalf("empty dir should be skipped, got %v", r.Roots)
	}
}

func TestNormalizeRootAbsolute(t *testing.T) {
	got, err := NormalizeRoot(t.TempDir())
	if err != nil {
		t.Fatalf("NormalizeRoot: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("normalized root %q should be absolute", got)
	}
	if _, err := NormalizeRoot(""); err == nil {
		t.Error("NormalizeRoot(\"\") should error")
	}
}
