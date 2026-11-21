//go:build linux

package containerjobs

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"syscall"
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
				chownLink: func(fd int, path string, uid, gid, flags int) error {
					links++
					var st unix.Stat_t
					err := unix.Fstat(fd, &st)
					if err != nil || st.Mode&unix.S_IFMT != unix.S_IFLNK || path != "" || flags != unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW || uid != 10001 || gid != 10001 {
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

func TestSymlinkHandoffRemainsBoundWhenCheckedNameIsReplaced(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("target", []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("target", "link"); err != nil {
		t.Fatal(err)
	}
	before, err := root.Lstat("link")
	if err != nil {
		t.Fatal(err)
	}
	inode := before.Sys().(*syscall.Stat_t).Ino
	called := false
	native := &Native{chownLink: func(fd int, path string, uid, gid, flags int) error {
		called = true
		// The interleaving occurs at the ownership syscall boundary, after check.
		if err := root.Remove("link"); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile("link", []byte("replacement"), 0600); err != nil {
			t.Fatal(err)
		}
		var checked unix.Stat_t
		if err := unix.Fstat(fd, &checked); err != nil {
			t.Fatal(err)
		}
		if checked.Ino != inode || checked.Mode&unix.S_IFMT != unix.S_IFLNK || path != "" || flags != unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW || uid != 10001 || gid != 10001 {
			t.Fatal("ownership redirected to replacement inode")
		}
		return nil
	}}
	if err := native.HandoffWorkspaceSymlink(root, "link"); err != nil || !called {
		t.Fatal("descriptor handoff failed", err)
	}
	replacement, err := root.ReadFile("link")
	if err != nil || string(replacement) != "replacement" {
		t.Fatal("replacement changed", err)
	}
	target, err := root.ReadFile("target")
	if err != nil || string(target) != "keep" {
		t.Fatal("symlink target changed", err)
	}
}

func TestNativeImportRootIsPrivateBeforeEntryCreationAndPublishedLast(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	calls := []int{}
	native := &Native{chownFile: func(file *os.File, uid, gid int) error {
		info, err := file.Stat()
		if err != nil || !info.IsDir() {
			t.Fatal("not root directory descriptor", err)
		}
		want, err := os.Stat(dir)
		if err != nil || !os.SameFile(info, want) {
			t.Fatal("wrong import root", err)
		}
		if uid != gid {
			t.Fatal("unexpected group", uid, gid)
		}
		calls = append(calls, uid)
		return nil
	}}
	if err := native.PrepareImportRoot(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 || len(calls) != 1 || calls[0] != 0 {
		t.Fatal("root was not made worker-private", err, calls)
	}
	if err := native.PublishImportRoot(root); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1] != 10001 {
		t.Fatal("root not handed off last", calls)
	}
	native.chownFile = func(*os.File, int, int) error { return errors.New("fake ownership failure") }
	if err := native.PrepareImportRoot(root); err == nil {
		t.Fatal("private staging failure ignored")
	}
	if err := native.PublishImportRoot(root); err == nil {
		t.Fatal("publication failure ignored")
	}
}

func TestPrivateImportRootRefusesModeFailure(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	native := &Native{chownFile: func(file *os.File, uid, gid int) error {
		if uid != 0 || gid != 0 {
			t.Fatal("not privileged staging")
		}
		return file.Close()
	}}
	if err := native.PrepareImportRoot(root); err == nil {
		t.Fatal("failed staging mode was ignored")
	}
}
