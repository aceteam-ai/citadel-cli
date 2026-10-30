//go:build !unix

package containerjobs

import "os"

func openRead(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
func openWrite(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_CREATE|os.O_WRONLY, 0600)
}

func archiveInode(os.FileInfo) string { return "" }
