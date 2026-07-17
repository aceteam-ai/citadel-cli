package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDeviceCreds(t *testing.T) {
	dir := t.TempDir()
	yaml := "device_api_token: dat_abc123\napi_base_url: https://aceteam.ai\norg_id: 00000000-0000-0000-0000-000000000000\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c := LoadDeviceCreds(dir)
	if c.Token != "dat_abc123" {
		t.Errorf("Token = %q, want dat_abc123", c.Token)
	}
	if c.APIBaseURL != "https://aceteam.ai" {
		t.Errorf("APIBaseURL = %q, want https://aceteam.ai", c.APIBaseURL)
	}
}

func TestLoadDeviceCreds_MissingFile(t *testing.T) {
	c := LoadDeviceCreds(t.TempDir())
	if c.Token != "" || c.APIBaseURL != "" {
		t.Errorf("missing file should yield empty creds, got %+v", c)
	}
}

func TestLoadDeviceCreds_PartialFile(t *testing.T) {
	dir := t.TempDir()
	// Token present, base URL absent (older config): caller falls back to its
	// own default for the base URL.
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("device_api_token: dat_only\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c := LoadDeviceCreds(dir)
	if c.Token != "dat_only" {
		t.Errorf("Token = %q, want dat_only", c.Token)
	}
	if c.APIBaseURL != "" {
		t.Errorf("APIBaseURL should be empty, got %q", c.APIBaseURL)
	}
}

func TestLoadDeviceCreds_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("not: [valid: yaml: {{{"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c := LoadDeviceCreds(dir)
	if c.Token != "" {
		t.Errorf("invalid yaml should yield empty creds, got %+v", c)
	}
}
