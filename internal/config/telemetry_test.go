package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultTelemetry(t *testing.T) {
	tel := DefaultTelemetry()
	if !tel.AnonTelemetryEnabled {
		t.Errorf("DefaultTelemetry should be opt-out (enabled), got %+v", tel)
	}
}

func TestLoadTelemetry_NoFile(t *testing.T) {
	dir := t.TempDir()
	tel := LoadTelemetry(dir)
	if !tel.AnonTelemetryEnabled {
		t.Errorf("LoadTelemetry with no file should return enabled default, got %+v", tel)
	}
}

func TestTelemetrySaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Opt out, then verify it persists.
	disabled := &Telemetry{AnonTelemetryEnabled: false}
	if err := SaveTelemetry(dir, disabled); err != nil {
		t.Fatalf("SaveTelemetry: %v", err)
	}

	loaded := LoadTelemetry(dir)
	if loaded.AnonTelemetryEnabled {
		t.Errorf("opt-out should persist: saved %+v, loaded %+v", disabled, loaded)
	}

	// Opt back in.
	enabled := &Telemetry{AnonTelemetryEnabled: true}
	if err := SaveTelemetry(dir, enabled); err != nil {
		t.Fatalf("SaveTelemetry: %v", err)
	}
	if !LoadTelemetry(dir).AnonTelemetryEnabled {
		t.Error("opt back in should persist as enabled")
	}
}

func TestSaveTelemetry_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	if err := SaveTelemetry(dir, DefaultTelemetry()); err != nil {
		t.Fatalf("SaveTelemetry should create nested dirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, telemetryFile)); err != nil {
		t.Errorf("telemetry file should exist after save: %v", err)
	}
}

func TestLoadTelemetry_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	data := []byte("not: [valid: yaml: {{{")
	if err := os.WriteFile(filepath.Join(dir, telemetryFile), data, 0644); err != nil {
		t.Fatalf("write invalid yaml: %v", err)
	}

	tel := LoadTelemetry(dir)
	if !tel.AnonTelemetryEnabled {
		t.Errorf("invalid YAML should return enabled default, got %+v", tel)
	}
}
