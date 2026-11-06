package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/apps"
	"github.com/aceteam-ai/citadel-cli/internal/cacheindex"
	"github.com/aceteam-ai/citadel-cli/internal/nvr"
	"github.com/aceteam-ai/citadel-cli/internal/rag"
	"github.com/aceteam-ai/citadel-cli/internal/source"
	"github.com/aceteam-ai/citadel-cli/internal/storage"
	"github.com/aceteam-ai/citadel-cli/internal/tmux"
	"github.com/aceteam-ai/citadel-cli/internal/update"
)

func TestInternalHomeDefaultsRemainInsideCommandFixture(t *testing.T) {
	home := t.TempDir()
	useCmdFixtureHome(t, home)
	foreign := t.TempDir()
	t.Setenv("HOME", foreign)
	t.Setenv("USERPROFILE", foreign)
	// These are independent explicit overrides, not OS-home resolution. Clear
	// them to test the home default while preserving their production precedence.
	for _, key := range []string{"CITADEL_WORKSPACE", "XDG_DATA_HOME", "LOCALAPPDATA", "APPDATA"} {
		t.Setenv(key, "")
	}
	for _, tc := range []struct {
		name, got, rel string
	}{
		{"cache", cacheindex.DefaultCacheRoot(), "citadel-cache"},
		{"git credentials", source.DefaultCredentialsDir(), "citadel-node/credentials"},
		{"rag workspace", rag.NodeWorkspaceDir(), "citadel-node/workspace"},
		{"update state", update.GetStateFilePath(), "citadel-node/update/state.json"},
	} {
		if want := filepath.Join(home, filepath.FromSlash(tc.rel)); tc.got != want {
			t.Fatalf("%s escaped the command fixture: %q, want %q", tc.name, tc.got, want)
		}
	}
	for _, tc := range []struct {
		name string
		get  func() (string, error)
		rel  string
	}{
		{"app", func() (string, error) { return apps.AppDir("synthetic") }, ".citadel/apps/synthetic"},
		{"storage", storage.BaseDir, ".citadel/storage"},
		{"NVR config", nvr.ConfigDir, ".citadel-cli/nvr/config"},
		{"NVR media", nvr.MediaDir, ".citadel-cli/nvr/media"},
	} {
		if got, err := tc.get(); err != nil || got != filepath.Join(home, filepath.FromSlash(tc.rel)) {
			t.Fatalf("%s escaped the command fixture: %q, %v", tc.name, got, err)
		}
	}
	binary := "tmux"
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if got := tmux.ManagedBinaryPath(); got != filepath.Join(home, ".citadel", "bin", binary) {
		t.Fatal("tmux path escaped command fixture")
	}
	roots := cacheindex.RuntimeStorageRoots("podman")
	if len(roots) != 1 || roots[0] != filepath.Join(home, ".local", "share", "containers", "storage") {
		t.Fatalf("rootless storage escaped home fixture: %v", roots)
	}
	// These getters must remain effect-free, including against the foreign
	// synthetic home selected by the environment.
	for _, root := range []string{home, foreign} {
		for _, rel := range []string{"citadel-cache", "citadel-node", ".citadel", ".citadel-cli/nvr"} {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
				t.Fatalf("path selection created %s before an explicit writer: %v", rel, err)
			}
		}
	}
}
