package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEgressRelayDefaultsOff(t *testing.T) {
	dir := t.TempDir()
	// No file present: default is OFF, LAN denied (opt-in / fail-closed).
	e := LoadEgressRelay(dir)
	if e.Enabled {
		t.Error("egress relay should default to disabled")
	}
	if e.AllowLAN {
		t.Error("egress relay allow_lan should default to false")
	}
}

func TestSaveLoadEgressRelayRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := SaveEgressRelay(dir, &EgressRelay{Enabled: true, AllowLAN: true}); err != nil {
		t.Fatalf("SaveEgressRelay: %v", err)
	}
	e := LoadEgressRelay(dir)
	if !e.Enabled {
		t.Error("expected relay enabled after save")
	}
	if !e.AllowLAN {
		t.Error("expected allow_lan enabled after save")
	}
	if _, err := os.Stat(filepath.Join(dir, egressRelayFile)); err != nil {
		t.Fatalf("egress-relay.yaml not written: %v", err)
	}
}

func TestLoadEgressRelayPartialFileKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	// A file that only sets "enabled" must leave allow_lan at its default
	// (false), not zero out the whole struct.
	content := "enabled: true\n"
	if err := os.WriteFile(filepath.Join(dir, egressRelayFile), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	e := LoadEgressRelay(dir)
	if !e.Enabled {
		t.Error("expected enabled=true from partial file")
	}
	if e.AllowLAN {
		t.Error("absent allow_lan key should keep default disabled")
	}
}

func TestEgressRelayLoadModifySavePreservesOtherField(t *testing.T) {
	dir := t.TempDir()
	if err := SaveEgressRelay(dir, &EgressRelay{Enabled: true, AllowLAN: false}); err != nil {
		t.Fatalf("SaveEgressRelay: %v", err)
	}

	// Load-modify-save only AllowLAN, mirroring how APPLY_DEVICE_CONFIG and
	// the local MCP tool apply a single field without clobbering the other.
	e := LoadEgressRelay(dir)
	e.AllowLAN = true
	if err := SaveEgressRelay(dir, e); err != nil {
		t.Fatalf("SaveEgressRelay: %v", err)
	}

	got := LoadEgressRelay(dir)
	if !got.Enabled {
		t.Error("expected Enabled to survive the load-modify-save of AllowLAN")
	}
	if !got.AllowLAN {
		t.Error("expected AllowLAN=true after modification")
	}
}
