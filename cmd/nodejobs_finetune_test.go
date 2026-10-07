package cmd

import (
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

func TestFineTuneModelCacheDirUsesCanonicalHFCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, "citadel-cache", services.HFHubCacheDirName)
	if got := fineTuneModelCacheDir(); got != want {
		t.Fatalf("fineTuneModelCacheDir() = %q, want %q", got, want)
	}
}
