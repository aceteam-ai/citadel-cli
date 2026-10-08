package network

import (
	"os"
	"testing"
)

func TestIdentityRecord_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := IdentityRecord{
		HeadscaleNodeID: "758",
		MeshIP:          "100.64.0.78",
		Hostname:        "ubuntu-gpu",
		StableID:        "sha256:abc123",
	}
	if err := SaveIdentityRecord(dir, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadIdentityRecord(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.HeadscaleNodeID != rec.HeadscaleNodeID || got.MeshIP != rec.MeshIP ||
		got.Hostname != rec.Hostname || got.StableID != rec.StableID {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, rec)
	}
	if got.RecordedAt == "" {
		t.Errorf("RecordedAt should be stamped on save")
	}
}

func TestIdentityRecord_FirstConnectBaselineNoError(t *testing.T) {
	got, err := LoadIdentityRecord(t.TempDir()) // no file yet
	if err != nil {
		t.Fatalf("first-connect load should not error: %v", err)
	}
	if got.HeadscaleNodeID != "" || got.StableID != "" {
		t.Errorf("expected empty baseline, got %+v", got)
	}
}

func TestIdentityRecord_CorruptDegradesToEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(identityRecordPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadIdentityRecord(dir)
	if err != nil {
		t.Fatalf("corrupt record should degrade, not error: %v", err)
	}
	if got.HeadscaleNodeID != "" {
		t.Errorf("corrupt record should yield an empty baseline, got %+v", got)
	}
}
