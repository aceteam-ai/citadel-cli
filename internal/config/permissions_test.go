package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPermissions(t *testing.T) {
	p := DefaultPermissions()
	if !p.Console || !p.Desktop || !p.Files || !p.Services || !p.SSH || !p.Shell {
		t.Errorf("DefaultPermissions should have all fields true, got %+v", p)
	}
}

func TestLoadPermissions_ShellDefaultsEnabled(t *testing.T) {
	// A pre-existing permissions file that predates the shell field must still
	// leave shell enabled (backward compatible: absent key keeps the default).
	dir := t.TempDir()
	data := []byte("console: true\nssh: false\n")
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), data, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	p := LoadPermissions(dir)
	if !p.Shell {
		t.Error("shell should default to enabled when absent from an older config file")
	}

	// Explicit opt-out must round-trip.
	data = []byte("shell: false\n")
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), data, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	p = LoadPermissions(dir)
	if p.Shell {
		t.Error("shell should be false when explicitly disabled in config")
	}
}

func TestLoadPermissions_NoFile(t *testing.T) {
	dir := t.TempDir()
	p := LoadPermissions(dir)
	if !p.Console || !p.Desktop || !p.Files || !p.Services || !p.SSH {
		t.Errorf("LoadPermissions with no file should return defaults, got %+v", p)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := &Permissions{
		Console:  false,
		Desktop:  true,
		Files:    false,
		Services: true,
		SSH:      false,
	}

	if err := SavePermissions(dir, original); err != nil {
		t.Fatalf("SavePermissions: %v", err)
	}

	loaded := LoadPermissions(dir)
	if loaded.Console != original.Console ||
		loaded.Desktop != original.Desktop ||
		loaded.Files != original.Files ||
		loaded.Services != original.Services ||
		loaded.SSH != original.SSH {
		t.Errorf("round trip mismatch: saved %+v, loaded %+v", original, loaded)
	}
}

func TestLoadPermissions_PartialConfig(t *testing.T) {
	dir := t.TempDir()

	// Write a partial file that only disables console
	data := []byte("console: false\n")
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), data, 0644); err != nil {
		t.Fatalf("write partial config: %v", err)
	}

	p := LoadPermissions(dir)
	if p.Console {
		t.Error("console should be false from partial config")
	}
	// All other fields should remain true (defaults)
	if !p.Desktop {
		t.Error("desktop should be true (default, not in partial config)")
	}
	if !p.Files {
		t.Error("files should be true (default, not in partial config)")
	}
	if !p.Services {
		t.Error("services should be true (default, not in partial config)")
	}
	if !p.SSH {
		t.Error("ssh should be true (default, not in partial config)")
	}
}

func TestSavePermissions_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	p := DefaultPermissions()

	if err := SavePermissions(dir, p); err != nil {
		t.Fatalf("SavePermissions should create nested dirs: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(filepath.Join(dir, "permissions.yaml")); err != nil {
		t.Errorf("permissions file should exist after save: %v", err)
	}
}

func TestLoadPermissions_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	data := []byte("not: [valid: yaml: {{{")
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), data, 0644); err != nil {
		t.Fatalf("write invalid yaml: %v", err)
	}

	p := LoadPermissions(dir)
	// Should still return defaults on parse error
	if !p.Console || !p.Desktop || !p.Files || !p.Services || !p.SSH {
		t.Errorf("invalid YAML should return defaults, got %+v", p)
	}
}
