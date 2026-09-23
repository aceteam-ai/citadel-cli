// internal/network/controlurl_test.go
package network

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadPersistedControlURL exercises the pure core against a t.TempDir(), so
// it never touches the machine-convergent GetNodeConfigDir() (which on a box
// running a live node resolves to that node's real config -- CLAUDE.md's
// ConfigDir()/GetNodeConfigDir() test rule).
func TestReadPersistedControlURL(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigYAML(t, dir, "nexus_url: https://nexus.box.internal\ndevice_api_token: tok\n")
		if got := readPersistedControlURL(dir); got != "https://nexus.box.internal" {
			t.Fatalf("got %q, want the persisted URL", got)
		}
	})

	t.Run("trims surrounding whitespace", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigYAML(t, dir, "nexus_url: \"  https://nexus.box.internal  \"\n")
		if got := readPersistedControlURL(dir); got != "https://nexus.box.internal" {
			t.Fatalf("got %q, want trimmed URL", got)
		}
	})

	t.Run("absent key yields empty", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigYAML(t, dir, "device_api_token: tok\norg_id: 42\n")
		if got := readPersistedControlURL(dir); got != "" {
			t.Fatalf("got %q, want empty for absent nexus_url", got)
		}
	})

	t.Run("missing file yields empty", func(t *testing.T) {
		if got := readPersistedControlURL(t.TempDir()); got != "" {
			t.Fatalf("got %q, want empty for missing config", got)
		}
	})

	t.Run("malformed yaml yields empty", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigYAML(t, dir, "\tnot: [valid: yaml")
		if got := readPersistedControlURL(dir); got != "" {
			t.Fatalf("got %q, want empty for malformed config", got)
		}
	})
}

// TestResolveControlURL pins the fallback contract: persisted wins, else the
// compiled-in default.
func TestResolveControlURL(t *testing.T) {
	if got := resolveControlURL("https://nexus.box.internal"); got != "https://nexus.box.internal" {
		t.Fatalf("persisted present: got %q, want the persisted URL", got)
	}
	if got := resolveControlURL(""); got != DefaultControlURL {
		t.Fatalf("persisted absent: got %q, want DefaultControlURL %q", got, DefaultControlURL)
	}
	if got := resolveControlURL("   "); got != DefaultControlURL {
		t.Fatalf("persisted blank: got %q, want DefaultControlURL %q", got, DefaultControlURL)
	}
}

func writeConfigYAML(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(contents), 0600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}
