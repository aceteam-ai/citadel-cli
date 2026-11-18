//go:build !windows

package botprofile

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// NodeShareFile is the node share's file name inside the store dir.
const NodeShareFile = "node-share.key"

// loadOrCreateNodeShare returns the 32-byte node share, creating it on first
// use. The store dir must be a real directory (not a symlink) owned by this
// user with no group/other access; the file must be a regular file (never a
// symlink) owned by this user with no group/other bits, exactly ShareLen
// bytes. A store failing any check is refused, never repaired: a loosened share
// is presumed read.
func loadOrCreateNodeShare(dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("botprofile: create store dir: %w", err)
	}
	var dst unix.Stat_t
	if err := unix.Lstat(dir, &dst); err != nil {
		return nil, fmt.Errorf("botprofile: stat store dir: %w", err)
	}
	if err := checkOwnerMode(&dst, unix.S_IFDIR, "store dir"); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, NodeShareFile)
	b, err := readShare(path)
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := createShare(dir, path); err != nil {
		return nil, err
	}
	return readShare(path)
}

func checkOwnerMode(st *unix.Stat_t, kind uint32, what string) error {
	if uint32(st.Mode)&unix.S_IFMT != kind {
		return fmt.Errorf("botprofile: node share %s is not the expected file type", what)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("botprofile: node share %s is not owned by the running user", what)
	}
	if st.Mode&0o077 != 0 {
		return fmt.Errorf("botprofile: node share %s is accessible to group or other (mode %o); refusing", what, uint32(st.Mode)&0o777)
	}
	return nil
}

func readShare(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("botprofile: open node share: %w", err)
	}
	f := os.NewFile(uintptr(fd), NodeShareFile)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("botprofile: stat node share: %w", err)
	}
	if err := checkOwnerMode(&st, unix.S_IFREG, "file"); err != nil {
		return nil, err
	}
	if st.Size != ShareLen {
		return nil, errors.New("botprofile: node share file has the wrong length; refusing")
	}
	b := make([]byte, ShareLen)
	if _, err := io.ReadFull(f, b); err != nil {
		return nil, fmt.Errorf("botprofile: read node share: %w", err)
	}
	return b, nil
}

// createShare writes a fresh random share to a temp file and links it into
// place, so a crash never leaves a partial share and a concurrent creator
// loses cleanly (the winner's file is then read).
func createShare(dir, path string) error {
	tmp, err := os.CreateTemp(dir, ".share-*")
	if err != nil {
		return fmt.Errorf("botprofile: create node share: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("botprofile: create node share: %w", err)
	}
	b := make([]byte, ShareLen)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return fmt.Errorf("botprofile: node share entropy: %w", err)
	}
	defer zero(b)
	if _, err := tmp.Write(b); err != nil {
		return fmt.Errorf("botprofile: write node share: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("botprofile: write node share: %w", err)
	}
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("botprofile: install node share: %w", err)
	}
	return nil
}
