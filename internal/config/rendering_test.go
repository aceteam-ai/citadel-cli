package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultRendering(t *testing.T) {
	r := DefaultRendering()
	if !r.Fullscreen {
		t.Errorf("DefaultRendering should be fullscreen (flicker-free is the expected experience), got %+v", r)
	}
}

func TestLoadRendering_NoFile(t *testing.T) {
	dir := t.TempDir()
	r := LoadRendering(dir)
	if !r.Fullscreen {
		t.Errorf("LoadRendering with no file should default to fullscreen, got %+v", r)
	}
}

func TestRenderingSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := &Rendering{Fullscreen: false}

	if err := SaveRendering(dir, original); err != nil {
		t.Fatalf("SaveRendering: %v", err)
	}

	loaded := LoadRendering(dir)
	if loaded.Fullscreen != original.Fullscreen {
		t.Errorf("round trip mismatch: saved %+v, loaded %+v", original, loaded)
	}
}

func TestLoadRendering_DisabledFromFile(t *testing.T) {
	dir := t.TempDir()
	data := []byte("fullscreen: false\n")
	if err := os.WriteFile(filepath.Join(dir, "rendering.yaml"), data, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	r := LoadRendering(dir)
	if r.Fullscreen {
		t.Error("fullscreen should be false from file")
	}
}

// Precedence: a partial file (an unrelated key present, fullscreen absent) must
// keep the default (true), because Unmarshal only overwrites keys present in the
// file. This mirrors LoadMouse's partial-file behavior.
func TestLoadRendering_PartialFileKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	data := []byte("unrelated: true\n")
	if err := os.WriteFile(filepath.Join(dir, "rendering.yaml"), data, 0644); err != nil {
		t.Fatalf("write partial config: %v", err)
	}

	r := LoadRendering(dir)
	if !r.Fullscreen {
		t.Errorf("partial file (fullscreen absent) should keep default true, got %+v", r)
	}
}

func TestLoadRendering_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	data := []byte("not: [valid: yaml: {{{")
	if err := os.WriteFile(filepath.Join(dir, "rendering.yaml"), data, 0644); err != nil {
		t.Fatalf("write invalid yaml: %v", err)
	}

	r := LoadRendering(dir)
	if !r.Fullscreen {
		t.Errorf("invalid YAML should return defaults (fullscreen), got %+v", r)
	}
}

func TestSaveRendering_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	if err := SaveRendering(dir, DefaultRendering()); err != nil {
		t.Fatalf("SaveRendering should create nested dirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "rendering.yaml")); err != nil {
		t.Errorf("rendering file should exist after save: %v", err)
	}
}
