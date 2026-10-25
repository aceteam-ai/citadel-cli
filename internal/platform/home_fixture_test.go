package platform

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestHomeFixtureIgnoresEnvironmentAndAccountLookup(t *testing.T) {
	resolverCapturedBeforeSetup := UserHomeDir
	home := t.TempDir()
	restore, err := OverrideHomeDirectoryForTesting(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restore(); err != nil {
			t.Error(err)
		}
	})
	foreign := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "SUDO_USER", "LOCALAPPDATA", "APPDATA"} {
		t.Setenv(key, foreign)
	}
	for _, name := range []string{"", "fixture-user-that-must-not-be-looked-up"} {
		if got, err := HomeDir(name); err != nil || got != home {
			t.Fatalf("home for %q escaped the explicit fixture: %q, %v", name, got, err)
		}
		if got, err := DefaultNodeDir(name); err != nil || got != filepath.Join(home, "citadel-node") {
			t.Fatalf("node fallback escaped the explicit fixture: %q, %v", got, err)
		}
	}
	if got, err := resolverCapturedBeforeSetup(); err != nil || got != home {
		t.Fatal("saved home resolver bypassed the later fixture installation")
	}
	// Readers share an immutable provider; no scope mutation runs in parallel.
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 100 {
				if got, err := HomeDir(""); err != nil || got != home {
					t.Error("concurrent reader lost its fixture")
					return
				}
			}
		}()
	}
	readers.Wait()
}

func TestHomeFixtureInvalidInstallKeepsPreviousAndRestoresInOrder(t *testing.T) {
	initial := homeDirectoryForTests.Load()
	a, b := t.TempDir(), t.TempDir()
	restoreA, err := OverrideHomeDirectoryForTesting(a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restoreA(); err != nil {
			t.Error(err)
		}
	})
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "relative", file, filepath.Join(t.TempDir(), "absent")} {
		if cleanup, err := OverrideHomeDirectoryForTesting(invalid); err == nil || cleanup != nil {
			t.Fatal("invalid home fixture accepted")
		}
		if got, _ := HomeDir(""); got != a {
			t.Fatal("invalid install replaced previous home fixture")
		}
	}
	restoreB, err := OverrideHomeDirectoryForTesting(b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restoreB(); err != nil {
			t.Error(err)
		}
	})
	if err := restoreA(); err == nil {
		t.Fatal("out-of-order restore succeeded")
	}
	if got, _ := HomeDir(""); got != b {
		t.Fatal("out-of-order restore replaced active home fixture")
	}
	if err := restoreB(); err != nil {
		t.Fatal(err)
	}
	if got, _ := HomeDir(""); got != a {
		t.Fatal("inner restore lost previous home fixture")
	}
	if err := restoreA(); err != nil {
		t.Fatal(err)
	}
	if err := restoreA(); err != nil || homeDirectoryForTests.Load() != initial {
		t.Fatal("home fixture did not restore idempotently")
	}
}

func TestHomeFixtureRejectsSymlink(t *testing.T) {
	home := t.TempDir()
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(home, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	before := homeDirectoryForTests.Load()
	if cleanup, err := OverrideHomeDirectoryForTesting(link); err == nil || cleanup != nil || homeDirectoryForTests.Load() != before {
		t.Fatal("symlink home fixture accepted or replaced previous scope")
	}
}
