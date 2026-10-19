// Package input opens selected offline inputs without following a checked
// regular path into a blocking FIFO. All names remain confined to an os.Root.
package input

import (
	"errors"
	"os"
)

var ErrNotRegular = errors.New("stream input is not a stable regular file")

// OpenRegular returns a read-only descriptor for the same regular file seen
// before opening it. Unix opens are nonblocking, so a concurrent FIFO swap is
// refused after opening instead of blocking before descriptor verification.
// The caller owns the returned descriptor. Symlinks selected by name, devices,
// directories and replacement objects are refused. os.Root owns confinement.
func OpenRegular(root *os.Root, name string) (*os.File, error) {
	return openRegular(root, name, openRead)
}

// The opener argument permits deterministic replacement tests without a global
// hook or timing-sensitive mutation loop. Production always uses openRead.
func openRegular(root *os.Root, name string, opener func(*os.Root, string) (*os.File, error)) (*os.File, error) {
	if root == nil { return nil, ErrNotRegular }
	before, err := root.Lstat(name)
	if err != nil { return nil, err }
	if !before.Mode().IsRegular() { return nil, ErrNotRegular }
	file, err := opener(root, name)
	if err != nil {
		if file != nil { file.Close() }
		return nil, err
	}
	if file == nil { return nil, ErrNotRegular }
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		file.Close()
		return nil, ErrNotRegular
	}
	return file, nil
}
