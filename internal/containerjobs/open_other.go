//go:build !unix

package containerjobs

import "os"

func openRead(root *os.Root, name string) (*os.File, error) { return root.Open(name) }

// Native container storage is unsupported on non-Unix platforms.
func openWrite(root *os.Root, name string) (*os.File, bool, error) {
	return nil, false, ErrUnavailable
}
func singleFileLink(os.FileInfo) bool                        { return false }
func openImportDirectory(*os.Root, string) (*os.File, error) { return nil, ErrUnavailable }

func archiveInode(os.FileInfo) string { return "" }
