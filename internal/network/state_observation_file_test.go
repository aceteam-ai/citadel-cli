package network

import (
	"os"
	"path/filepath"
	"testing"
)

func TestObserveStatePresenceMissingEmptyAndPopulated(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "missing", "network")
	if present, err := ObserveStatePresence(stateDir); present || err != nil {
		t.Fatalf("missing suffix = %t, %v", present, err)
	}
	if _, err := os.Lstat(filepath.Dir(stateDir)); !os.IsNotExist(err) {
		t.Fatal("observation created the missing directory")
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if present, err := ObserveStatePresence(stateDir); present || err != nil {
		t.Fatalf("empty directory = %t, %v", present, err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "tailscaled.state"), []byte("PRIVATE_KEY"), 0600); err != nil {
		t.Fatal(err)
	}
	if present, err := ObserveStatePresence(stateDir); !present || err != nil {
		t.Fatalf("populated directory = %t, %v", present, err)
	}
	after, err := os.Stat(stateDir)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatal("observation replaced the directory or changed its mode")
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "tailscaled.state" {
		t.Fatal("observation changed state contents")
	}
}

func TestObserveStatePresenceWrongTypeIsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PRIVATE_PATH")
	if err := os.WriteFile(path, []byte("PRIVATE_TOKEN"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, stateDir := range []string{"", "relative", path, filepath.Join(path, "network")} {
		present, err := ObserveStatePresence(stateDir)
		if present || err != errStateObservation {
			t.Fatalf("invalid state location = %t, %v", present, err)
		}
	}
}

func TestReadStateObservationFileBoundedAndUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if data, exists, err := readStateObservationFile(path, 3); len(data) != 0 || exists || err != nil {
		t.Fatalf("missing file = %q, %t, %v", data, exists, err)
	}
	if err := os.WriteFile(path, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, exists, err := readStateObservationFile(path, 3)
	if err != nil || !exists || string(data) != "abc" {
		t.Fatalf("bounded file = %q, %t, %v", data, exists, err)
	}
	if _, exists, err := readStateObservationFile(path, 2); exists || err != errStateObservation {
		t.Fatal("oversized input must refuse, not fall back")
	}
	after, err := os.Stat(path)
	if err != nil || !sameStateObservation(before, after) {
		t.Fatal("read-only observation mutated file identity, mode or metadata")
	}
	if _, _, err := readStateObservationFile(filepath.Dir(path), 3); err != errStateObservation {
		t.Fatal("directory must not be read as configuration")
	}
}
