//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package input

import "os"

func openRead(root *os.Root, name string) (*os.File, error) {
	// Windows os.Root uses nonblocking handles and rejects non-local/device
	// paths. Other non-Unix platforms have no Unix FIFO open semantics.
	return root.OpenFile(name, os.O_RDONLY, 0)
}
