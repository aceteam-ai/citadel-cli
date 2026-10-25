package cmd

import (
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

func TestFineTuneModelCacheDirUsesCanonicalHFCache(t *testing.T) {
	home := t.TempDir()
	useCmdFixtureHome(t, home)
	want := filepath.Join(home, "citadel-cache", services.HFHubCacheDirName)
	if got := fineTuneModelCacheDir(); got != want {
		t.Fatalf("fineTuneModelCacheDir() = %q, want %q", got, want)
	}
}
