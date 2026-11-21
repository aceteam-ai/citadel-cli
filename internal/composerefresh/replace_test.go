package composerefresh

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceManagedComposeRejectsChangedInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ollama.yml")
	if err := os.WriteFile(path, []byte("operator changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceManagedCompose(path, "ollama", "expected", "selected"); err == nil {
		t.Fatal("raced edit overwritten")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "operator changed" {
		t.Fatal("raced content lost")
	}
}

func TestReplaceManagedComposeRepairsStampWithoutChangingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ollama.yml")
	if err := os.WriteFile(path, []byte("selected"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeStamp(dir, managedStamp{Version: "unchanged", Hashes: map[string]string{"ollama": "old", "vllm": "peer"}}); err != nil {
		t.Fatal(err)
	}
	changed, err := ReplaceManagedCompose(path, "ollama", "selected", "selected")
	if err != nil || changed {
		t.Fatalf("repair: %v %v", changed, err)
	}
	stamp, ok := readStamp(dir)
	if !ok || stamp.Version != "unchanged" || stamp.Hashes["ollama"] != hashContent("selected") || stamp.Hashes["vllm"] != "peer" {
		t.Fatalf("bad repaired stamp: %+v", stamp)
	}
}

func TestReplaceManagedComposeDoesNotInventStamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ollama.yml")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := ReplaceManagedCompose(path, "ollama", "old", "selected")
	if err != nil || !changed {
		t.Fatalf("replace: %v %v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, StampFileName)); !os.IsNotExist(err) {
		t.Fatalf("invented stamp: %v", err)
	}
}
