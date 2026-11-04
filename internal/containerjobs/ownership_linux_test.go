//go:build linux

package containerjobs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeOwnershipUsesPinnedIDsOnDescriptorAndSymlink(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file")
			if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := root.Symlink("file", "link"); err != nil {
				t.Fatal(err)
			}
			files, links := 0, 0
			native := &Native{
				chownFile: func(got *os.File, uid, gid int) error {
					files++
					if got != file || uid != 10001 || gid != 10001 {
						t.Fatal("file descriptor/IDs not pinned", uid, gid)
					}
					if fail {
						return errors.New("fake refusal")
					}
					return nil
				},
				chownLink: func(got *os.Root, name string, uid, gid int) error {
					links++
					if got != root || name != "link" || uid != 10001 || gid != 10001 {
						t.Fatal("symlink root/IDs not pinned", uid, gid)
					}
					if fail {
						return errors.New("fake refusal")
					}
					return nil
				},
			}
			if err := native.HandoffWorkspaceFile(file); (err != nil) != fail {
				t.Fatal("file ownership error lost", err)
			}
			if err := native.HandoffWorkspaceSymlink(root, "link"); (err != nil) != fail {
				t.Fatal("link ownership error lost", err)
			}
			if files != 1 || links != 1 {
				t.Fatal("ownership syscall seam bypassed", files, links)
			}
			if err := native.HandoffWorkspaceSymlink(root, "file"); err == nil {
				t.Fatal("regular file accepted as symlink")
			}
			if links != 1 {
				t.Fatal("invalid symlink reached metadata syscall")
			}
		})
	}
}

func TestNativeOwnershipRefusesHardlinkedFileBeforeChown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	native := &Native{chownFile: func(*os.File, int, int) error { t.Fatal("hardlink reached ownership syscall"); return nil }}
	if err := native.HandoffWorkspaceFile(file); err == nil {
		t.Fatal("hardlinked file handed off")
	}
}

func TestNativeDirectoryOwnershipUsesPinnedDescriptor(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	called := false
	native := &Native{chownFile: func(got *os.File, uid, gid int) error {
		called = true
		if got != dir || uid != 10001 || gid != 10001 {
			t.Fatal("directory descriptor/IDs not pinned")
		}
		return nil
	}}
	if err := native.HandoffWorkspaceFile(dir); err != nil || !called {
		t.Fatal("directory handoff missing", err)
	}
	if err := dir.Close(); err != nil {
		t.Fatal(err)
	}
	called = false
	if err := native.HandoffWorkspaceFile(dir); err == nil || called {
		t.Fatal("closed descriptor reached ownership syscall")
	}
}
