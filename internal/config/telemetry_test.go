package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultTelemetry(t *testing.T) {
	tel := DefaultTelemetry()
	if !tel.AnonTelemetryEnabled {
		t.Errorf("DefaultTelemetry should be enabled (opt-out), got %+v", tel)
	}
}

func TestLoadTelemetry_NoFile(t *testing.T) {
	dir := t.TempDir()
	tel := LoadTelemetry(dir)
	if !tel.AnonTelemetryEnabled {
		t.Errorf("LoadTelemetry with no file should default to enabled, got %+v", tel)
	}
}

func TestTelemetrySaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := &Telemetry{AnonTelemetryEnabled: false}

	if err := SaveTelemetry(dir, original); err != nil {
		t.Fatalf("SaveTelemetry: %v", err)
	}

	loaded := LoadTelemetry(dir)
	if loaded.AnonTelemetryEnabled != original.AnonTelemetryEnabled {
		t.Errorf("round trip mismatch: saved %+v, loaded %+v", original, loaded)
	}
}

func TestLoadTelemetry_DisabledFromFile(t *testing.T) {
	dir := t.TempDir()
	data := []byte("anon_telemetry_enabled: false\n")
	if err := os.WriteFile(filepath.Join(dir, "telemetry.yaml"), data, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tel := LoadTelemetry(dir)
	if tel.AnonTelemetryEnabled {
		t.Error("anon_telemetry_enabled should be false from file")
	}
}

func TestLoadTelemetry_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	data := []byte("not: [valid: yaml: {{{")
	if err := os.WriteFile(filepath.Join(dir, "telemetry.yaml"), data, 0644); err != nil {
		t.Fatalf("write invalid yaml: %v", err)
	}

	tel := LoadTelemetry(dir)
	if !tel.AnonTelemetryEnabled {
		t.Errorf("invalid YAML should return defaults (enabled), got %+v", tel)
	}
}

func TestSaveTelemetry_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	if err := SaveTelemetry(dir, DefaultTelemetry()); err != nil {
		t.Fatalf("SaveTelemetry should create nested dirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "telemetry.yaml")); err != nil {
		t.Errorf("telemetry file should exist after save: %v", err)
	}
}
