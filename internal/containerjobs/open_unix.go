//go:build unix

package containerjobs

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

func openRead(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
}
func openWrite(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_CREATE|os.O_WRONLY|unix.O_NONBLOCK, 0600)
}

func archiveInode(info os.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return fmt.Sprintf("%x:%x", uint64(st.Dev), uint64(st.Ino))
	}
	return ""
}
