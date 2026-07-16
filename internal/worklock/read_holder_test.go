package worklock

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadHolderNoFile verifies ReadHolder reports ok=false when no lock file
// exists, so a node with no worker never yields a bogus holder record.
func TestReadHolderNoFile(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")
	if rec, ok := ReadHolder(stateDir); ok {
		t.Fatalf("ReadHolder on missing lock file = ok (%+v), want not ok", rec)
	}
}

// TestReadHolderParsesRecord verifies ReadHolder returns the recorded PID, start
// time, and version from a JSON lock record, WITHOUT checking liveness (it is the
// metadata companion to IsHeld). A fabricated far-future PID would be dead, but
// ReadHolder must still report it.
func TestReadHolderParsesRecord(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")
	path := LockPathForStateDir(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	rec := lockRecord{PID: 4242, StartUnix: 1700000000, Version: "2.75.0"}
	if err := os.WriteFile(path, encodeRecord(rec), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok := ReadHolder(stateDir)
	if !ok {
		t.Fatal("ReadHolder returned ok=false for a valid record")
	}
	if got.PID != 4242 {
		t.Errorf("PID = %d, want 4242", got.PID)
	}
	if got.Version != "2.75.0" {
		t.Errorf("Version = %q, want 2.75.0", got.Version)
	}
	if got.StartTime.Unix() != 1700000000 {
		t.Errorf("StartTime = %d, want 1700000000", got.StartTime.Unix())
	}
}

// TestReadHolderEmptyRecord verifies an empty/garbage lock file (PID <= 0) reports
// not ok, matching the reclaim classification (a garbage record is not a holder).
func TestReadHolderEmptyRecord(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")
	path := LockPathForStateDir(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec, ok := ReadHolder(stateDir); ok {
		t.Fatalf("ReadHolder on empty record = ok (%+v), want not ok", rec)
	}
}
