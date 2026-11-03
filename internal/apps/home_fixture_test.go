package apps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestAppStateWriterUsesHomeFixtureAfterSelectorsChange(t *testing.T) {
	home, foreign := t.TempDir(), t.TempDir()
	restore, err := platform.OverrideHomeDirectoryForTesting(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restore(); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("HOME", foreign)
	t.Setenv("USERPROFILE", foreign)
	want := filepath.Join(home, ".citadel", "apps", "state.json")
	if got, err := statePath(); err != nil || got != want {
		t.Fatal("app state path escaped fixture before the first read/write")
	}
	state, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.path != want {
		t.Fatal("loaded state captured a foreign path before Save")
	}
	state.Set(InstalledApp{Name: "synthetic", Image: "fixture.invalid/synthetic", HostPort: 8103})
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("positive control did not save owned state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(foreign, ".citadel")); !os.IsNotExist(err) {
		t.Fatal("app writer touched foreign home")
	}
}
