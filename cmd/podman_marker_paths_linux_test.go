//go:build linux

package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPodmanMarkerFixtureCheckedBeforeExclusiveWrite(t *testing.T) {
	dir := t.TempDir()
	restore, err := overridePodmanMarkerDirectoryForTesting(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restore(); err != nil {
			t.Error(err)
		}
	})
	foreign := filepath.Join(t.TempDir(), "rootless-worker-user")
	if err := os.WriteFile(foreign, []byte("foreign synthetic marker\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Dir(foreign))
	t.Setenv("SUDO_USER", "fixture-user-that-must-not-be-looked-up")
	path := podmanUserMarkerPath()
	if path != filepath.Join(dir, "rootless-worker-user") {
		t.Fatal("marker path escaped fixture before any marker I/O")
	}
	verified := false
	verify := func(selected string) error {
		if selected != dir {
			t.Fatal("writer verified a directory outside its owned fixture")
		}
		verified = true
		return nil // Fixture-only: never alter/chown a real account or directory.
	}
	if err := writePodmanUserMarkerAt(path, "1001", verify); err != nil {
		t.Fatal(err)
	}
	if !verified {
		t.Fatal("marker writer skipped directory validation")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "1001\n" {
		t.Fatalf("positive control did not write the owned marker: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("marker must retain mode 0600: %v", err)
	}
	if err := writePodmanUserMarkerAt(path, "1002", verify); err == nil {
		t.Fatal("exclusive marker writer overwrote an existing marker")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "1001\n" {
		t.Fatal("exclusive-create refusal modified the owned marker")
	}
	if got, err := os.ReadFile(foreign); err != nil || string(got) != "foreign synthetic marker\n" {
		t.Fatal("foreign marker tripwire changed")
	}
}

func TestPodmanMarkerValidationFailurePrecedesFileCreation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rootless-worker-user")
	denied := errors.New("synthetic directory validation refusal")
	err := writePodmanUserMarkerAt(path, "1001", func(got string) error {
		if got != dir {
			t.Fatal("unexpected validation path")
		}
		return denied
	})
	if !errors.Is(err, denied) {
		t.Fatalf("directory refusal lost: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("marker was created before directory validation")
	}
}

func TestPodmanMarkerReaderRejectsInvalidFixtureWithoutNativeAudit(t *testing.T) {
	dir := t.TempDir()
	restore, err := overridePodmanMarkerDirectoryForTesting(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restore(); err != nil {
			t.Error(err)
		}
	})
	path := podmanUserMarkerPath()
	if path != filepath.Join(dir, "rootless-worker-user") {
		t.Fatal("marker reader path escaped fixture before any read")
	}
	// Invalid even when tests run as root. This provenance-only helper cannot
	// invoke id/sudo; under non-root the directory ownership check denies first.
	if err := os.WriteFile(path, []byte("invalid synthetic provenance\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyPodmanUserMarkerAt(path, "1001"); err == nil {
		t.Fatal("invalid fixture marker passed provenance validation")
	}
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := verifyPodmanMarkerDir(); err == nil {
		t.Fatal("writable fixture marker directory passed the production verifier")
	}
}

func TestPodmanMarkerFixtureRejectsInvalidInstallAndRestoresInOrder(t *testing.T) {
	before := podmanUserMarkerPath()
	a, b := t.TempDir(), t.TempDir()
	restoreA, err := overridePodmanMarkerDirectoryForTesting(a)
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
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err) // This file runs on Linux only.
	}
	for _, invalid := range []string{"", "relative", file, link, filepath.Join(t.TempDir(), "absent")} {
		if cleanup, err := overridePodmanMarkerDirectoryForTesting(invalid); err == nil || cleanup != nil {
			t.Fatal("invalid marker fixture accepted")
		}
		if podmanUserMarkerPath() != filepath.Join(a, "rootless-worker-user") {
			t.Fatal("failed install replaced marker provider")
		}
	}
	restoreB, err := overridePodmanMarkerDirectoryForTesting(b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restoreB(); err != nil {
			t.Error(err)
		}
	})
	if err := restoreA(); err == nil || podmanUserMarkerPath() != filepath.Join(b, "rootless-worker-user") {
		t.Fatal("out-of-order restore removed active marker provider")
	}
	if err := restoreB(); err != nil || podmanUserMarkerPath() != filepath.Join(a, "rootless-worker-user") {
		t.Fatal("inner restore lost previous marker provider")
	}
	if err := restoreA(); err != nil {
		t.Fatal(err)
	}
	if err := restoreA(); err != nil || podmanUserMarkerPath() != before {
		t.Fatal("marker provider did not restore idempotently")
	}
}
