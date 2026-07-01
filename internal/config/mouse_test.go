package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultMouse(t *testing.T) {
	m := DefaultMouse()
	if !m.Enabled {
		t.Errorf("DefaultMouse should be enabled (mouse is the feature), got %+v", m)
	}
}

func TestLoadMouse_NoFile(t *testing.T) {
	dir := t.TempDir()
	m := LoadMouse(dir)
	if !m.Enabled {
		t.Errorf("LoadMouse with no file should default to enabled, got %+v", m)
	}
}

func TestMouseSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := &Mouse{Enabled: false}

	if err := SaveMouse(dir, original); err != nil {
		t.Fatalf("SaveMouse: %v", err)
	}

	loaded := LoadMouse(dir)
	if loaded.Enabled != original.Enabled {
		t.Errorf("round trip mismatch: saved %+v, loaded %+v", original, loaded)
	}
}

func TestLoadMouse_DisabledFromFile(t *testing.T) {
	dir := t.TempDir()
	data := []byte("enabled: false\n")
	if err := os.WriteFile(filepath.Join(dir, "mouse.yaml"), data, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	m := LoadMouse(dir)
	if m.Enabled {
		t.Error("enabled should be false from file")
	}
}

func TestLoadMouse_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	data := []byte("not: [valid: yaml: {{{")
	if err := os.WriteFile(filepath.Join(dir, "mouse.yaml"), data, 0644); err != nil {
		t.Fatalf("write invalid yaml: %v", err)
	}

	m := LoadMouse(dir)
	if !m.Enabled {
		t.Errorf("invalid YAML should return defaults (enabled), got %+v", m)
	}
}

func TestSaveMouse_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	if err := SaveMouse(dir, DefaultMouse()); err != nil {
		t.Fatalf("SaveMouse should create nested dirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mouse.yaml")); err != nil {
		t.Errorf("mouse file should exist after save: %v", err)
	}
}
