package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultKeepAwake(t *testing.T) {
	k := DefaultKeepAwake()
	if k.KeepAwakeOnAC {
		t.Errorf("DefaultKeepAwake should be disabled (opt-in), got %+v", k)
	}
}

func TestLoadKeepAwake_NoFile(t *testing.T) {
	dir := t.TempDir()
	k := LoadKeepAwake(dir)
	if k.KeepAwakeOnAC {
		t.Errorf("LoadKeepAwake with no file should default to disabled, got %+v", k)
	}
}

func TestKeepAwakeSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := &KeepAwake{KeepAwakeOnAC: true}

	if err := SaveKeepAwake(dir, original); err != nil {
		t.Fatalf("SaveKeepAwake: %v", err)
	}

	loaded := LoadKeepAwake(dir)
	if loaded.KeepAwakeOnAC != original.KeepAwakeOnAC {
		t.Errorf("round trip mismatch: saved %+v, loaded %+v", original, loaded)
	}
}

func TestLoadKeepAwake_EnabledFromFile(t *testing.T) {
	dir := t.TempDir()
	data := []byte("keep_awake_on_ac: true\n")
	if err := os.WriteFile(filepath.Join(dir, "keepawake.yaml"), data, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	k := LoadKeepAwake(dir)
	if !k.KeepAwakeOnAC {
		t.Error("keep_awake_on_ac should be true from file")
	}
}

func TestLoadKeepAwake_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	data := []byte("not: [valid: yaml: {{{")
	if err := os.WriteFile(filepath.Join(dir, "keepawake.yaml"), data, 0644); err != nil {
		t.Fatalf("write invalid yaml: %v", err)
	}

	k := LoadKeepAwake(dir)
	if k.KeepAwakeOnAC {
		t.Errorf("invalid YAML should return defaults (disabled), got %+v", k)
	}
}

func TestSaveKeepAwake_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	if err := SaveKeepAwake(dir, DefaultKeepAwake()); err != nil {
		t.Fatalf("SaveKeepAwake should create nested dirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keepawake.yaml")); err != nil {
		t.Errorf("keepawake file should exist after save: %v", err)
	}
}
