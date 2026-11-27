//go:build linux || darwin

package harnessseat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLocalSeatDirectoryExplicitCreationAndReadOnlyReopen(t *testing.T) {
	i := fixtureIdentity(t, Codex)
	i.ConfigDir = filepath.Join(i.HomeDir, "private-seats", "first")
	if _, err := PrepareLocalSeatDirectory(i, false); !errors.Is(err, ErrSeatDirectory) {
		t.Fatal("read created missing directory")
	}
	if _, err := os.Stat(filepath.Join(i.HomeDir, "private-seats")); !os.IsNotExist(err) {
		t.Fatal("read made parent")
	}
	dir, err := PrepareLocalSeatDirectory(i, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Check(); err != nil {
		t.Fatal(err)
	}
	if err := dir.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dir.Check(); !errors.Is(err, ErrSeatDirectory) {
		t.Fatal("closed pin accepted")
	}
	for _, path := range []string{filepath.Dir(i.ConfigDir), i.ConfigDir} {
		info, err := os.Stat(path)
		if err != nil || info.Mode() != os.ModeDir|0o700 {
			t.Fatal("new path is not private")
		}
	}
	marker := filepath.Join(i.ConfigDir, "untouched.fixture")
	if err := os.WriteFile(marker, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareLocalSeatDirectory(i, true); !errors.Is(err, ErrSeatDirectoryExists) {
		t.Fatal("add silently adopted existing directory")
	}
	reopened, err := PrepareLocalSeatDirectory(i, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := os.ReadFile(marker); err != nil || string(got) != "unchanged" {
		t.Fatal("validation changed vendor file")
	}
}

func TestLocalSeatDirectoryRejectsUnsafePathsWithoutRepair(t *testing.T) {
	for _, kind := range []string{"home-symlink", "parent-symlink", "leaf-symlink", "writable-home", "writable-parent", "public-leaf", "file-leaf"} {
		t.Run(kind, func(t *testing.T) {
			i := fixtureIdentity(t, Codex)
			parent := filepath.Join(i.HomeDir, "seats")
			i.ConfigDir = filepath.Join(parent, "first")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "home-symlink":
				link := filepath.Join(t.TempDir(), "linked-home")
				if err := os.Symlink(i.HomeDir, link); err != nil {
					t.Fatal(err)
				}
				i.HomeDir, i.ConfigDir = link, filepath.Join(link, "seats", "first")
			case "parent-symlink":
				link := filepath.Join(i.HomeDir, "linked-parent")
				if err := os.Symlink(parent, link); err != nil {
					t.Fatal(err)
				}
				i.ConfigDir = filepath.Join(link, "first")
			case "leaf-symlink":
				if err := os.Symlink(t.TempDir(), i.ConfigDir); err != nil {
					t.Fatal(err)
				}
			case "writable-home":
				if err := os.Chmod(i.HomeDir, 0o777); err != nil {
					t.Fatal(err)
				}
			case "writable-parent":
				if err := os.Chmod(parent, 0o770); err != nil {
					t.Fatal(err)
				}
			case "public-leaf":
				if err := os.Mkdir(i.ConfigDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(i.ConfigDir, 0o755); err != nil {
					t.Fatal(err)
				}
			case "file-leaf":
				if err := os.WriteFile(i.ConfigDir, []byte("unchanged"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, create := range []bool{false, true} {
				if dir, err := PrepareLocalSeatDirectory(i, create); err == nil {
					dir.Close()
					t.Fatal("unsafe path accepted")
				}
			}
			if kind == "public-leaf" {
				info, err := os.Stat(i.ConfigDir)
				if err != nil || info.Mode().Perm() != 0o755 {
					t.Fatal("permissions repaired")
				}
			}
			if kind == "file-leaf" {
				if got, err := os.ReadFile(i.ConfigDir); err != nil || string(got) != "unchanged" {
					t.Fatal("file replaced")
				}
			}
		})
	}
}

func TestLocalSeatDirectoryAllowsSafeSharedParentButNotPublicLeaf(t *testing.T) {
	i := fixtureIdentity(t, Muse)
	parent := filepath.Join(i.HomeDir, "config")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	i.ConfigDir = filepath.Join(parent, "isolated")
	dir, err := PrepareLocalSeatDirectory(i, true)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := os.Chmod(i.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := dir.Check(); !errors.Is(err, ErrSeatDirectory) {
		t.Fatal("lost privacy accepted")
	}
}

func TestLocalSeatDirectoryPinDoesNotFollowReplacement(t *testing.T) {
	i := fixtureIdentity(t, Codex)
	dir, err := PrepareLocalSeatDirectory(i, true)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	oldPath := i.ConfigDir + "-original"
	if err := os.Rename(i.ConfigDir, oldPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), i.ConfigDir); err != nil {
		t.Fatal(err)
	}
	// Revalidation refuses a replaced pathname instead of granting authority
	// from a still-open old inode to a new path-based child.
	if err := dir.Check(); !errors.Is(err, ErrSeatDirectory) {
		t.Fatal("replacement pathname accepted")
	}
	if reopened, err := PrepareLocalSeatDirectory(i, false); err == nil {
		reopened.Close()
		t.Fatal("replacement path adopted")
	}
}

func TestLocalSeatDirectoryBoundsWorkBeforeCreatingParents(t *testing.T) {
	i := fixtureIdentity(t, Codex)
	i.ConfigDir = filepath.Join(i.HomeDir, strings.Repeat("child/", maxSeatDirectoryDepth+1), "leaf")
	if _, err := PrepareLocalSeatDirectory(i, true); !errors.Is(err, ErrSeatDirectory) {
		t.Fatal("unbounded path traversal accepted")
	}
	entries, err := os.ReadDir(i.HomeDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("over-budget path created parents")
	}
}

func TestLocalSeatDirectoryOwnershipPredicateRefusesForeignUID(t *testing.T) {
	i := fixtureIdentity(t, Codex)
	info, err := os.Stat(i.HomeDir)
	if err != nil {
		t.Fatal(err)
	}
	if !ownedSeatParentInfo(info) {
		t.Fatal("fixture owner rejected")
	}
	stat := *info.Sys().(*syscall.Stat_t)
	stat.Uid++
	if ownedSeatParentInfo(saltOwnerFixture{info, stat}) {
		t.Fatal("foreign owner accepted")
	}
}
