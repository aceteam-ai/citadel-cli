// Package renameio is a minimal, cross-platform reimplementation of the public
// API of github.com/google/renameio (v1) that this repo pulls in transitively
// via github.com/coder/hnsw.
//
// Why this patch exists: upstream renameio v1's TempFile/PendingFile are
// gated `//go:build !windows`, so coder/hnsw's encode.go (which calls
// renameio.TempFile in its Graph.Save path) fails to COMPILE on Windows —
// breaking `GOOS=windows go build ./cmd/citadel`, a platform citadel supports.
// Citadel never calls hnsw's Save/Export (it uses the in-memory graph only), so
// this drop-in just needs to compile everywhere and behave correctly if ever
// called. It is wired via `replace github.com/google/renameio => ./patches/renameio`
// in the root go.mod, mirroring the existing patches/tcell replace. coder/hnsw
// is the sole importer of the v1 module path in the whole dependency graph
// (tailscale uses renameio/v2, a different module path this replace does not
// touch), so the blast radius is exactly coder/hnsw.
//
// Implementation is os.CreateTemp + atomic os.Rename (Go's os.Rename replaces an
// existing destination on Windows via MoveFileEx), which is portable.
//
// Upstream renameio is BSD-3-Clause (see LICENSE in this directory).
package renameio

import (
	"os"
	"path/filepath"
)

// PendingFile is a temporary file that can be atomically renamed into place.
// It embeds *os.File so callers can write to it directly (e.g. bufio.NewWriter).
type PendingFile struct {
	*os.File

	path   string // final destination path
	done   bool   // CloseAtomicallyReplace succeeded
	closed bool   // underlying file closed
}

// TempDir returns the directory temporary files should be created in for an
// atomic replacement of dest: the directory containing dest, so the final
// rename stays on the same filesystem.
func TempDir(dest string) string {
	return filepath.Dir(dest)
}

// TempFile creates a temporary file intended to be atomically renamed to path.
// When dir is empty, the temp file is created alongside path (same directory),
// so CloseAtomicallyReplace is a same-filesystem rename.
func TempFile(dir, path string) (*PendingFile, error) {
	if dir == "" {
		dir = filepath.Dir(path)
	}
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return nil, err
	}
	return &PendingFile{File: f, path: path}, nil
}

// closeFile closes the underlying file once.
func (t *PendingFile) closeFile() error {
	if t.closed {
		return nil
	}
	t.closed = true
	return t.File.Close()
}

// Cleanup is a no-op if CloseAtomicallyReplace succeeded, and otherwise closes
// and removes the temporary file. Not safe for concurrent use.
func (t *PendingFile) Cleanup() error {
	if t.done {
		return nil
	}
	err := t.closeFile()
	if rerr := os.Remove(t.Name()); err == nil {
		err = rerr
	}
	return err
}

// CloseAtomicallyReplace closes the temporary file and atomically renames it to
// the destination path. Not safe for concurrent use.
func (t *PendingFile) CloseAtomicallyReplace() error {
	if err := t.File.Sync(); err != nil {
		return err
	}
	if err := t.closeFile(); err != nil {
		return err
	}
	if err := os.Rename(t.Name(), t.path); err != nil {
		return err
	}
	t.done = true
	return nil
}

// WriteFile atomically writes data to filename by writing to a temp file in the
// same directory and renaming it into place.
func WriteFile(filename string, data []byte, perm os.FileMode) error {
	t, err := TempFile("", filename)
	if err != nil {
		return err
	}
	defer t.Cleanup()
	if _, err := t.Write(data); err != nil {
		return err
	}
	if err := t.Chmod(perm); err != nil {
		return err
	}
	return t.CloseAtomicallyReplace()
}

// Symlink atomically creates or replaces the symlink newname pointing to
// oldname by creating a temporary symlink and renaming it into place.
func Symlink(oldname, newname string) error {
	tmp := newname + ".renameio-tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(oldname, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, newname); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
