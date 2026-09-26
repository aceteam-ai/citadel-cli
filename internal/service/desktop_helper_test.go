package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallDesktopHelperSurvivesSourceMove(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "Citadel.app", "Contents", "MacOS", "citadel-aarch64-apple-darwin")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("version one"), 0o700); err != nil {
		t.Fatal(err)
	}
	installed, err := installDesktopHelper(source, home)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(installed, "/Library/Application Support/ai.aceteam.citadel/helpers/") {
		t.Fatalf("unexpected helper path %q", installed)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(installed); err != nil || string(data) != "version one" {
		t.Fatalf("installed helper after source removal: %q, %v", data, err)
	}
	if err := os.WriteFile(source, []byte("version two"), 0o700); err != nil {
		t.Fatal(err)
	}
	next, err := installDesktopHelper(source, home)
	if err != nil || next == installed {
		t.Fatalf("new bundle must install a distinct version: %q, %v", next, err)
	}
	if data, err := os.ReadFile(installed); err != nil || string(data) != "version one" {
		t.Fatalf("old version changed: %q, %v", data, err)
	}
}
