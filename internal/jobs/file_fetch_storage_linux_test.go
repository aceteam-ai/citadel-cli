//go:build linux

package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFileFetchStorageAnonymousInodeAndProcFDPublication(t *testing.T) {
	workspace := t.TempDir()
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	staging, err := fileFetchOpenStaging(root)
	if err != nil {
		t.Fatal(err)
	}
	defer staging.Close()
	if _, err := staging.WriteString("verified bytes"); err != nil {
		t.Fatal(err)
	}
	var inode unix.Stat_t
	if err := unix.Fstat(int(staging.Fd()), &inode); err != nil || inode.Nlink != 0 || inode.Mode&0777 != 0600 {
		t.Fatalf("staging is not an unnamed private inode: nlink=%d mode=%o err=%v", inode.Nlink, inode.Mode, err)
	}
	if entries, err := os.ReadDir(workspace); err != nil || len(entries) != 0 {
		t.Fatalf("unpublished staging is visible: %v %v", entries, err)
	}
	dir, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	// Exercise the /proc/self/fd path explicitly even on a privileged test host
	// where the preferred AT_EMPTY_PATH operation might work without fallback.
	if err := fileFetchLinkProcFD(staging, dir, "published"); err != nil {
		t.Fatal(err)
	}
	source, err := staging.Stat()
	if err != nil {
		t.Fatal(err)
	}
	linked, err := root.Stat("published")
	if err != nil || !os.SameFile(source, linked) {
		t.Fatalf("publication did not link the open staging inode: %v", err)
	}
	if data, err := root.ReadFile("published"); err != nil || string(data) != "verified bytes" {
		t.Fatalf("unexpected published content: %q %v", data, err)
	}
	if err := fileFetchLinkProcFD(staging, dir, "published"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("proc FD publication did not refuse existing destination: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(outside, []byte("outside unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink(outside, "symlink"); err != nil {
		t.Fatal(err)
	}
	if err := fileFetchLinkProcFD(staging, dir, "symlink"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("proc FD publication followed/replaced destination symlink: %v", err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "outside unchanged" {
		t.Fatalf("symlink target changed: %q %v", data, err)
	}
}

func TestFileFetchStorageProbeLeavesWorkspaceClean(t *testing.T) {
	workspace := t.TempDir()
	if !fileFetchStorageSupported(workspace) {
		t.Fatal("anonymous creation and FD publication not available")
	}
	if entries, err := os.ReadDir(workspace); err != nil || len(entries) != 0 {
		t.Fatalf("storage probe left a visible entry: %v %v", entries, err)
	}
}

func TestFileFetchStorageClosedDescriptorFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	staging, err := fileFetchOpenStaging(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := staging.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fileFetchPublish(root, staging, "published"); err == nil {
		t.Fatal("closed anonymous descriptor published")
	}
	if entries, err := os.ReadDir(workspace); err != nil || len(entries) != 0 {
		t.Fatalf("failed publication left a visible entry: %v %v", entries, err)
	}
}
