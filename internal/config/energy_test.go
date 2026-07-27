package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnergyDefaultsOff(t *testing.T) {
	dir := t.TempDir()
	// No file present: default is OFF (opt-in).
	if e := LoadEnergy(dir); e.SamplingEnabled {
		t.Fatal("energy sampling should default to disabled")
	}
}

func TestSaveLoadEnergyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := SaveEnergy(dir, &Energy{SamplingEnabled: true}); err != nil {
		t.Fatalf("SaveEnergy: %v", err)
	}
	if e := LoadEnergy(dir); !e.SamplingEnabled {
		t.Fatal("expected sampling enabled after save")
	}
	// File is named energy.yaml.
	if _, err := os.Stat(filepath.Join(dir, energyFile)); err != nil {
		t.Fatalf("energy.yaml not written: %v", err)
	}
}

func TestLoadEnergyPartialFileKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	// A file that does not mention sampling_enabled leaves the default (false).
	if err := os.WriteFile(filepath.Join(dir, energyFile), []byte("# empty\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if e := LoadEnergy(dir); e.SamplingEnabled {
		t.Fatal("absent key should keep default disabled")
	}
}
