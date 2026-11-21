//go:build !windows

package platform

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPrivateSettingsOpenRefusesSymlinkAndFIFO(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenPrivateFile(regular)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenPrivateFile(link); err == nil {
		_ = f.Close()
		t.Fatal("symlink followed")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := OpenPrivateFile(fifo)
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted")
		}
	case <-time.After(time.Second):
		// Release a mutated blocking reader before failing, so no orphaned
		// goroutine or special-file reader survives the fake-fixture test.
		fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			_ = unix.Close(fd)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("settings open blocked on FIFO")
	}
}

func TestPrivateSettingsReadJudgesDescriptorPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openPrivateNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if validatePrivateHandle(f, st) == nil {
		t.Fatal("descriptor with public read permission accepted")
	}
}
