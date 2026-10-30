package devicemode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestDeviceServicePathsUseHomeFixtureWithoutInstalling(t *testing.T) {
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
	for _, tc := range []struct {
		get func() (string, error)
		rel string
	}{
		{launchdPlistPath, filepath.Join("Library", "LaunchAgents", LaunchdLabel+".plist")},
		{systemdUnitPath, filepath.Join(".config", "systemd", "user", SystemdUnitName)},
	} {
		if got, err := tc.get(); err != nil || got != filepath.Join(home, tc.rel) {
			t.Fatalf("device service path escaped fixture: %q, %v", got, err)
		}
	}
	for _, root := range []string{home, foreign} {
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatal("device path selection had installation effects")
		}
	}
}
