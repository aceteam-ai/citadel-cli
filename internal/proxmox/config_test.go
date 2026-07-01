package proxmox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPath(t *testing.T) {
	dir := "/home/user/.citadel-cli"
	got := ConfigPath(dir)
	want := filepath.Join(dir, "proxmox.json")
	if got != want {
		t.Fatalf("ConfigPath(%q) = %q, want %q", dir, got, want)
	}
}

func TestDeleteConfig_RemovesExistingFile(t *testing.T) {
	dir := t.TempDir()

	cfg := &Config{BaseURL: "https://192.168.2.4:8006", NodeName: "pve"}
	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	path := ConfigPath(dir)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected config file to exist before delete: %v", err)
	}

	if err := DeleteConfig(dir); err != nil {
		t.Fatalf("DeleteConfig: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected config file to be removed, stat err = %v", err)
	}
}

func TestDeleteConfig_NotExistIsSuccess(t *testing.T) {
	dir := t.TempDir()

	// No config file was ever written; deleting it should be a no-op success.
	if err := DeleteConfig(dir); err != nil {
		t.Fatalf("DeleteConfig on missing file should succeed, got: %v", err)
	}

	// Calling it twice remains a success.
	if err := DeleteConfig(dir); err != nil {
		t.Fatalf("second DeleteConfig should still succeed, got: %v", err)
	}
}
