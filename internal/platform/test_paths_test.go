package platform

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConfigFixtureIgnoresEnvironmentAndReturnsOnlyOwnedCandidate(t *testing.T) {
	dir := t.TempDir()
	restore, err := OverrideConfigDirectoryForTesting(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restore(); err != nil {
			t.Error(err)
		}
	})
	for _, key := range []string{"HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "SUDO_USER"} {
		t.Setenv(key, "fixture-selector-must-not-be-used")
	}
	if got := ConfigDir(); got != dir {
		t.Fatalf("config directory = %q", got)
	}
	if got := ConfigDirCandidates(); !reflect.DeepEqual(got, []string{dir}) {
		t.Fatalf("candidates escaped fixture: %v", got)
	}
	candidates := ConfigDirCandidates()
	candidates[0] = "mutated caller copy"
	if got := ConfigDirCandidates(); !reflect.DeepEqual(got, []string{dir}) {
		t.Fatal("candidate mutation changed the installed provider")
	}
}

func TestConfigFixtureRejectsInvalidDirectoriesWithoutReplacingScope(t *testing.T) {
	dir := t.TempDir()
	restore, err := OverrideConfigDirectoryForTesting(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restore(); err != nil {
			t.Error(err)
		}
	})
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "relative", file, filepath.Join(t.TempDir(), "missing")} {
		if cleanup, err := OverrideConfigDirectoryForTesting(invalid); err == nil || cleanup != nil {
			t.Fatal("invalid fixture accepted")
		}
		if ConfigDir() != dir {
			t.Fatal("invalid install replaced the provider")
		}
	}
	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(dir, link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if cleanup, err := OverrideConfigDirectoryForTesting(link); err == nil || cleanup != nil {
			t.Fatal("symlink fixture accepted")
		}
		if ConfigDir() != dir {
			t.Fatal("symlink rejection replaced the provider")
		}
	})
}

func TestConfigFixtureRestoreIsOrderedAndIdempotent(t *testing.T) {
	initial := configDirectoryForTests.Load()
	a, b := t.TempDir(), t.TempDir()
	restoreA, err := OverrideConfigDirectoryForTesting(a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restoreA(); err != nil {
			t.Error(err)
		}
	})
	restoreB, err := OverrideConfigDirectoryForTesting(b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restoreB(); err != nil {
			t.Error(err)
		}
	})
	if err := restoreA(); err == nil || ConfigDir() != b {
		t.Fatal("out-of-order restore removed the active scope")
	}
	if err := restoreB(); err != nil || ConfigDir() != a {
		t.Fatal("inner restore lost the previous fixture")
	}
	if err := restoreA(); err != nil {
		t.Fatal(err)
	}
	if err := restoreA(); err != nil || configDirectoryForTests.Load() != initial {
		t.Fatal("original provider not restored idempotently")
	}
}
