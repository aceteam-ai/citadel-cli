//go:build !windows

package botprofile

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, fmt.Errorf("botprofile: create store dir: %w", err)
	}
	// First use publishes the whole store atomically: a private temp dir
	// holding the share is renamed to the store name, so a store dir without a
	// share is never a legitimate transient state. A loser of the race removes
	// its temp dir and reads the winner's share.
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if err := publishNewStore(dir); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("botprofile: stat store dir: %w", err)
	}
	var dst unix.Stat_t
	if err := unix.Lstat(dir, &dst); err != nil {
		return nil, fmt.Errorf("botprofile: stat store dir: %w", err)
	}
	if err := checkOwnerMode(&dst, unix.S_IFDIR, "store dir"); err != nil {
		return nil, err
	}
	removeStaleTemps(dir)
	path := filepath.Join(dir, NodeShareFile)
	b, err := readShareSettled(path)
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// A store dir without a share is not ours to fill in: it was planted or
	// left by a crash. Recovery: delete bot-profiles/ (nothing is sealed under
	// a share that does not exist, so nothing is lost).
	return nil, ErrStoreWithoutShare
}

// publishNewStore creates dir containing a fresh share, atomically. If another
// caller published first, it discards its own copy and returns nil.
func publishNewStore(dir string) error {
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".botstore-*")
	if err != nil {
		return fmt.Errorf("botprofile: create store dir: %w", err)
	}
	defer os.RemoveAll(tmp) // no-op after a successful rename
	if err := os.Chmod(tmp, 0o700); err != nil {
		return fmt.Errorf("botprofile: create store dir: %w", err)
	}
	if err := createShare(tmp, filepath.Join(tmp, NodeShareFile)); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		if errors.Is(err, os.ErrExist) || errors.Is(err, unix.ENOTEMPTY) {
			return nil // lost the race; read the winner's share
		}
		return fmt.Errorf("botprofile: publish store: %w", err)
	}
	return nil
}

// readShareSettled reads the share, tolerating the creation window in which
// the temp name and the final name are both linked (link count 2). A share that
// stays multiply linked is refused: a second name is a second route to the key.
func readShareSettled(path string) ([]byte, error) {
	for i := 0; ; i++ {
		b, err := readShare(path)
		if !errors.Is(err, errShareLinked) || i >= 5 {
			if errors.Is(err, errShareLinked) {
				return nil, errors.New("botprofile: node share has extra hard links; refusing")
			}
			return b, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var errShareLinked = errors.New("linked")

// removeStaleTemps deletes temp names left by a creator that died between
// create and link. Recent ones are left alone: a live creator owns them.
func removeStaleTemps(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".share-") {
			continue
		}
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) > time.Minute {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
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
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("botprofile: open node share: %w", err)
	}
	// O_NONBLOCK: opening a FIFO for reading must not wait for a writer. The
	// type check below refuses it; regular-file reads never block.
	f := os.NewFile(uintptr(fd), NodeShareFile)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("botprofile: stat node share: %w", err)
	}
	if err := checkOwnerMode(&st, unix.S_IFREG, "file"); err != nil {
		return nil, err
	}
	if uint64(st.Nlink) != 1 {
		return nil, errShareLinked
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
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Someone else installed a share between our read and our link. We
			// never adopt a share this process did not create: the caller may
			// retry, which re-reads it through the normal checks.
			return ErrNodeShareContested
		}
		return fmt.Errorf("botprofile: install node share: %w", err)
	}
	return nil
}
