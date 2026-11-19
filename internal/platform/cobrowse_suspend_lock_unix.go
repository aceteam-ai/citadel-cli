//go:build !windows

package platform

import (
	"golang.org/x/sys/unix"
	"os"
)

// The stable lock inode is never unlinked. Closing the handle releases the
// nonblocking OS lock, including after a process crash or exec.
func lockSuspendManifestStore(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
